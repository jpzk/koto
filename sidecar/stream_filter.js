#!/usr/bin/env node
// Filter claude's --output-format stream-json into raw streaming text.
// Each text_delta is written verbatim to stdout the moment it arrives.
// Before the first text chunk we emit `[ts:<epoch-ms>]\n` so the daemon
// tail/history parsers can attach an accurate timestamp to the response
// block (otherwise history replay can only stamp emit-time, which is
// minutes-to-days off for old log entries).
// One trailing newline at message_stop so the TUI can distinguish turns.
// fs.writeSync(OUT, ...) bypasses Node's stdout buffering; without it,
// small writes coalesce and streaming visibility disappears.
const fs = require('fs');
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
// KOTO_SUBAGENT: sub-agent mode (driven by cs-subagent). Same fd contract as
// venice_stream.js's VENICE_ONESHOT: the streamed framing (text/thinking
// deltas, tool calls) is progress, not the return value, so it goes to
// stderr — under cs-job both fds land in the job's out file, so the job view
// streams live — and only the final `result` record's text is written to
// stdout. The filter is the last command in cs-subagent's pipeline, so its
// exit code is the job's: 1 when the stream ends without a successful result
// (claude crashed, or the result record carries is_error).
const SUB = !!process.env.KOTO_SUBAGENT;
const OUT = SUB ? 2 : 1;
let gotResult = false;
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
  if (!stamped) { fs.writeSync(OUT, `[ts:${Date.now()}]\n`); stamped = true; }
}
function breakLine() {
  if (midline) { fs.writeSync(OUT, '\n'); midline = false; }
}

// Emit a framed tool output block. Body may be a string or an array of
// {type:"text",text:...} parts; non-text parts (images, etc.) are skipped.
// Empty body still emits the markers so the daemon tailer sees an explicit
// "empty result" event rather than missing the call's output entirely.
//
// Body lines that look like our own close markers (`[[think_end]] N`,
// `[[tool_out_end]] N`) are escaped with a leading backslash. The daemon
// tailer's nesting rule already prevents a body containing `[[*_begin]]`
// or a non-matching `[[*_end]]` from misparsing — only the matching close
// marker for the currently-open block is a risk. Escaping those line
// prefixes closes that last hole. A user who runs e.g.
// `echo '[[tool_out_end]] 0'` will see `\[[tool_out_end]] 0` rendered,
// which is a tiny visual artifact in exchange for the framing being
// unforgeable from inside tool output.
function escapeBody(s) {
  return s.replace(/^(\[\[(?:think_end|tool_out_end)\]\] )/gm, '\\$1');
}

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
  const safe = escapeBody(body);
  fs.writeSync(OUT, '[[tool_out_begin]]\n');
  if (safe.length) {
    fs.writeSync(OUT, safe.endsWith('\n') ? safe : safe + '\n');
  }
  fs.writeSync(OUT, `[[tool_out_end]] ${bytes}\n`);
}

// Session pinning: every stream-json record carries the run's session_id.
// Capture the first one into $KOTO_SESSION_ID_FILE (set by entrypoint.sh to
// /workspace/.cs/sessions/<name>.id) so the next turn of this chat session
// can `claude --resume <id>` the same conversation. Written once per run —
// a single -p invocation has a single session id.
const idFile = process.env.KOTO_SESSION_ID_FILE || '';
let wroteId = false;

rl.on('line', (line) => {
  let ev;
  try { ev = JSON.parse(line); } catch { return; }
  if (idFile && !wroteId && typeof ev.session_id === 'string' && ev.session_id) {
    try { fs.writeFileSync(idFile, ev.session_id); wroteId = true; } catch {}
  }
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
  // The run's final `result` record. In subagent mode this IS the return
  // value: its text goes to stdout (everything above went to stderr). In
  // normal mode it stays ignored — the streamed deltas already carried it.
  if (ev.type === 'result') {
    if (SUB) {
      if (!ev.is_error && typeof ev.result === 'string') {
        const ans = ev.result;
        fs.writeSync(1, ans.endsWith('\n') || !ans.length ? ans : ans + '\n');
        gotResult = true;
      } else {
        breakLine();
        fs.writeSync(OUT, `[[err]] claude: ${ev.subtype || 'error'}\n`);
      }
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
      fs.writeSync(OUT, '[[think_begin]]\n');
    }
    return;
  }
  if (e.type === 'content_block_delta' && e.delta) {
    if (e.delta.type === 'text_delta' && e.delta.text) {
      stampOnce();
      fs.writeSync(OUT, e.delta.text);
      midline = !e.delta.text.endsWith('\n');
    } else if (e.delta.type === 'thinking_delta' && thinking[e.index] && e.delta.thinking) {
      // Stream thinking text into the log as-is. The daemon's tailer is
      // stateful: anything between [[think_begin]] and [[think_end]] is
      // emitted as `event:"thinking_stream"` (partial) / `"thinking"` (line).
      fs.writeSync(OUT, e.delta.thinking);
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
      fs.writeSync(OUT, `[[tool]] ${t.name} ${t.inputBuf || '{}'}\n`);
    } else if (thinking[e.index]) {
      const t = thinking[e.index];
      delete thinking[e.index];
      const words = (t.wordsBuf.trim().split(/\s+/).filter(Boolean)).length;
      breakLine();
      fs.writeSync(OUT, `[[think_end]] ${words}\n`);
    }
    return;
  }
  if (e.type === 'message_stop') {
    fs.writeSync(OUT, '\n');
    midline = false;
  }
});

// Stream ended without a successful result record → the claude process died
// (or errored) before finishing. Surface that as the subagent's exit code so
// cs-job records rc!=0 instead of a silent empty success.
rl.on('close', () => {
  if (SUB && !gotResult) process.exitCode = 1;
});
