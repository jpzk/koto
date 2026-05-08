#!/usr/bin/env node
// Filter claude's --output-format stream-json: emit each text_delta as a line.
// Ignores housekeeping events (system/init, message_start, etc).
// Result: incremental text chunks in stdout, one chunk per line, picked up
// by the tail -F that the TUI's tail thread runs.
const readline = require('readline');
const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
  let ev;
  try { ev = JSON.parse(line); } catch { return; }
  if (ev.type === 'stream_event' && ev.event && ev.event.type === 'content_block_delta') {
    const d = ev.event.delta;
    if (d && d.type === 'text_delta' && d.text) {
      process.stdout.write(d.text + '\n');
    }
  } else if (ev.type === 'result' && ev.subtype && ev.subtype !== 'success') {
    process.stderr.write(`[stream error: ${ev.subtype}] ${ev.error || ''}\n`);
  }
});
