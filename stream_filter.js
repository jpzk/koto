#!/usr/bin/env node
// Filter claude's --output-format stream-json into raw streaming text.
// Each text_delta is written verbatim to stdout the moment it arrives.
// One trailing newline at message_stop so the TUI can distinguish turns.
// fs.writeSync(1, ...) bypasses Node's stdout buffering; without it,
// small writes coalesce and streaming visibility disappears.
const fs = require('fs');
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
  let ev;
  try { ev = JSON.parse(line); } catch { return; }
  if (ev.type !== 'stream_event' || !ev.event) return;
  const e = ev.event;
  if (e.type === 'content_block_delta' && e.delta && e.delta.type === 'text_delta' && e.delta.text) {
    fs.writeSync(1, e.delta.text);
  } else if (e.type === 'message_stop') {
    fs.writeSync(1, '\n');
  }
});
