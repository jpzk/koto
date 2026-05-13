#!/usr/bin/env node
// Filter claude's --output-format stream-json into raw streaming text.
// Each text_delta is written verbatim to stdout the moment it arrives.
// Before the first text chunk we emit `[ts:<epoch-ms>]\n` so the daemon
// tail/history parsers can attach an accurate timestamp to the response
// block (otherwise history replay can only stamp emit-time, which is
// minutes-to-days off for old log entries).
// One trailing newline at message_stop so the TUI can distinguish turns.
// fs.writeSync(1, ...) bypasses Node's stdout buffering; without it,
// small writes coalesce and streaming visibility disappears.
const fs = require('fs');
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
let stamped = false;
// "midline" means we've emitted content and not yet emitted a trailing \n,
// so the next marker needs a separator first. Toggled by writes.
let midline = false;
// Track block state by index. tool_use has {name, inputBuf}; thinking has
// {wordsBuf, chars} — wordsBuf accumulates so we can emit a word count
// summary on stop.
const tools = {};
const thinking = {};

function stampOnce() {
  if (!stamped) { fs.writeSync(1, `[ts:${Date.now()}]\n`); stamped = true; }
}
function breakLine() {
  if (midline) { fs.writeSync(1, '\n'); midline = false; }
}

// Emit a framed tool output block. Body may be a string or an array of
// {type:"text",text:...} parts; non-text parts (images, etc.) are skipped.
// Empty body still emits the markers so the daemon tailer sees an explicit
// "empty result" event rather than missing the call's output entirely.
function emitToolOut(content) {
  let body = '';
  if (typeof content === 'string') {
    body = content;
  } else if (Array.isArray(content)) {
    for (const p of content) {
      if (p && p.type === 'text' && typeof p.text === 'string') body += p.text;
    }
  }
  stampOnce();
  breakLine();
  const bytes = Buffer.byteLength(body, 'utf8');
  fs.writeSync(1, '[[tool_out_begin]]\n');
  if (body.length) {
    fs.writeSync(1, body.endsWith('\n') ? body : body + '\n');
  }
  fs.writeSync(1, `[[tool_out_end]] ${bytes}\n`);
}

rl.on('line', (line) => {
  let ev;
  try { ev = JSON.parse(line); } catch { return; }
  // Tool results are injected by the harness (not the model), so claude-code
  // emits them as a top-level `user` record containing tool_result content
  // blocks — NOT inside the assistant's stream_event partials. Handle this
  // record type before the stream_event gate.
  if (ev.type === 'user' && ev.message && Array.isArray(ev.message.content)) {
    for (const block of ev.message.content) {
      if (block && block.type === 'tool_result') emitToolOut(block.content);
    }
    return;
  }
  if (ev.type !== 'stream_event' || !ev.event) return;
  const e = ev.event;
  if (e.type === 'content_block_start' && e.content_block) {
    if (e.content_block.type === 'tool_use') {
      tools[e.index] = { name: e.content_block.name || 'tool', inputBuf: '' };
    } else if (e.content_block.type === 'thinking') {
      thinking[e.index] = { wordsBuf: '' };
      stampOnce();
      breakLine();
      // Open a framed region; subsequent thinking_delta text is the body.
      fs.writeSync(1, '[[think_begin]]\n');
    }
    return;
  }
  if (e.type === 'content_block_delta' && e.delta) {
    if (e.delta.type === 'text_delta' && e.delta.text) {
      stampOnce();
      fs.writeSync(1, e.delta.text);
      midline = !e.delta.text.endsWith('\n');
    } else if (e.delta.type === 'thinking_delta' && thinking[e.index] && e.delta.thinking) {
      // Stream thinking text into the log as-is. The daemon's tailer is
      // stateful: anything between [[think_begin]] and [[think_end]] is
      // emitted as `event:"thinking_stream"` (partial) / `"thinking"` (line).
      fs.writeSync(1, e.delta.thinking);
      thinking[e.index].wordsBuf += e.delta.thinking;
      midline = !e.delta.thinking.endsWith('\n');
    } else if (e.delta.type === 'input_json_delta' && tools[e.index]) {
      tools[e.index].inputBuf += e.delta.partial_json || '';
    }
    return;
  }
  if (e.type === 'content_block_stop') {
    if (tools[e.index]) {
      const t = tools[e.index];
      delete tools[e.index];
      stampOnce();
      breakLine();
      fs.writeSync(1, `[[tool]] ${t.name} ${t.inputBuf || '{}'}\n`);
    } else if (thinking[e.index]) {
      const t = thinking[e.index];
      delete thinking[e.index];
      const words = (t.wordsBuf.trim().split(/\s+/).filter(Boolean)).length;
      breakLine();
      fs.writeSync(1, `[[think_end]] ${words}\n`);
    }
    return;
  }
  if (e.type === 'message_stop') {
    fs.writeSync(1, '\n');
    midline = false;
  }
});
