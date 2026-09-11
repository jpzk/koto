#!/usr/bin/env node
// Filter claude's --output-format stream-json into [[marker]] streaming text.
//
// ONLY cs-subagent uses this now (KOTO_SUBAGENT mode: a sub-agent's progress
// framing into a cs-job out file, which JobTail parses with the same grammar).
// Chat turns no longer pass through here: fc-agent (fcguest/turn.go) decodes
// stream-json itself and sends typed TurnFrames over vsock; the daemon
// renders the marker text. Keep the two in step when the grammar changes.
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
// Bounds on what ONE streamed block may make this process accumulate (audit
// 2026-09-11 L151). Neither buffer had a budget of any kind: readline hands
// over a whole line before this sees it, and both of these then grew for the
// life of a block that a model — or a subagent run an authenticated caller can
// invoke — chooses the length of. The tool input additionally becomes a single
// [[tool]] line the daemon parses, the transcript stores and every client
// carries.
const TOOL_INPUT_MAX = 64 * 1024;
const WORDS_BUF_MAX = 256 * 1024;

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
      // wordsBuf exists only to COUNT words at the end, so past the cap it
      // stops growing and the count is reported as approximate (audit
      // 2026-09-11 L151). It used to accumulate the entire thinking block —
      // a second full copy of it, in a process that has already written every
      // byte straight out.
      // Stream thinking text into the log as-is. The daemon's tailer is
      // stateful: anything between [[think_begin]] and [[think_end]] is
      // emitted as `event:"thinking_stream"` (partial) / `"thinking"` (line).
      fs.writeSync(OUT, e.delta.thinking);
      if (thinking[e.index].wordsBuf.length < WORDS_BUF_MAX) {
        thinking[e.index].wordsBuf += e.delta.thinking;
      } else {
        thinking[e.index].clipped = true;
      }
      midline = !e.delta.thinking.endsWith('\n');
    } else if (e.delta.type === 'input_json_delta' && tools[e.index]) {
      // The tool-input buffer is the one that goes into the log as a single
      // [[tool]] LINE, so it is bounded at the size a summary line can be
      // (audit 2026-09-11 L151). Past the cap the fragments are dropped and
      // the marker says the JSON is clipped, rather than the process holding
      // an unbounded string and writing an unbounded line for the daemon's
      // parser, the transcript and every client to carry.
      if (tools[e.index].inputBuf.length < TOOL_INPUT_MAX) {
        tools[e.index].inputBuf += e.delta.partial_json || '';
      } else {
        tools[e.index].clipped = true;
      }
    }
    return;
  }
  if (e.type === 'content_block_stop') {
    if (tools[e.index]) {
      const t = tools[e.index];
      delete tools[e.index];
      stampOnce();
      breakLine();
      const input = t.clipped ? `${t.inputBuf}…[clipped]` : (t.inputBuf || '{}');
      fs.writeSync(OUT, `[[tool]] ${t.name} ${input}\n`);
    } else if (thinking[e.index]) {
      const t = thinking[e.index];
      delete thinking[e.index];
      const words = (t.wordsBuf.trim().split(/\s+/).filter(Boolean)).length;
      breakLine();
      fs.writeSync(OUT, `[[think_end]] ${words}${t.clipped ? '+' : ''}\n`);
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
