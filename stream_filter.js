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
rl.on('line', (line) => {
  let ev;
  try { ev = JSON.parse(line); } catch { return; }
  if (ev.type !== 'stream_event' || !ev.event) return;
  const e = ev.event;
  if (e.type === 'content_block_delta' && e.delta && e.delta.type === 'text_delta' && e.delta.text) {
    if (!stamped) {
      fs.writeSync(1, `[ts:${Date.now()}]\n`);
      stamped = true;
    }
    fs.writeSync(1, e.delta.text);
  } else if (e.type === 'message_stop') {
    fs.writeSync(1, '\n');
  }
});
