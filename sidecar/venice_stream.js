#!/usr/bin/env node
// Venice provider runner.
//
// Reads inputs from env (passed by entrypoint.sh):
//   MSG_B64       base64-encoded user message (already augmented by daemon
//                 with the <clawson-context> block).
//   SP_B64        base64-encoded system prompt (composeSystemPrompt output);
//                 empty if no global/per-group prompt is configured.
//   VENICE_MODEL  model name (e.g. venice-uncensored). Defaults applied in
//                 entrypoint.sh, not here.
//
// Conversation state lives in /workspace/.cs/venice-history.json — Venice's
// API is stateless, so we replay the full transcript on every turn. The
// daemon's clearCmd deletes this file when /clear is invoked.
//
// Output format matches the Claude path so the daemon's log tailer
// (tailLog in daemon.go) parses both providers identically:
//   [ts:<epoch-ms>]\n   — stamped once before the first text byte
//   <text deltas>       — written verbatim as SSE chunks arrive
//   \n                  — a final newline so the last partial line flushes
//                         as a `done` event instead of staying a `stream`.
//
// Errors are written as `[[err]] <msg>\n` so they surface in the TUI with
// the red glyph instead of being mistaken for assistant content.

const fs = require('fs');
const http = require('http');
const https = require('https');
const url = require('url');

const HISTORY = '/workspace/.cs/venice-history.json';
const BASE = process.env.ANTHROPIC_BASE_URL; // proxy URL; both providers reuse it
const MODEL = process.env.VENICE_MODEL || 'venice-uncensored';

function decodeB64(s) {
  if (!s) return '';
  try { return Buffer.from(s, 'base64').toString('utf8'); } catch { return ''; }
}
const USER_MSG = decodeB64(process.env.MSG_B64);
const SYS_PROMPT = decodeB64(process.env.SP_B64);

function writeErr(s) {
  try { fs.writeSync(1, `[[err]] ${s}\n`); } catch {}
}

if (!BASE) {
  writeErr('venice: ANTHROPIC_BASE_URL not set');
  process.exit(0);
}
if (!USER_MSG) {
  writeErr('venice: empty MSG_B64');
  process.exit(0);
}

let history = [];
try {
  const raw = fs.readFileSync(HISTORY, 'utf8');
  const parsed = JSON.parse(raw);
  if (Array.isArray(parsed)) history = parsed;
} catch {
  // Missing or unparseable history file → start fresh. /clear wipes it; a
  // brand-new group never had one.
}

const messages = [];
if (SYS_PROMPT) messages.push({ role: 'system', content: SYS_PROMPT });
for (const m of history) {
  if (m && typeof m.role === 'string' && typeof m.content === 'string') {
    messages.push({ role: m.role, content: m.content });
  }
}
messages.push({ role: 'user', content: USER_MSG });

const body = JSON.stringify({
  model: MODEL,
  messages,
  stream: true,
});

// Parse BASE — proxy is plain HTTP inside the container network.
const parsed = url.parse(BASE + '/api/v1/chat/completions');
const opts = {
  protocol: parsed.protocol,
  hostname: parsed.hostname,
  port: parsed.port,
  path: parsed.path,
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'Authorization': 'Bearer proxied', // sentinel; proxy replaces with real key
    'Content-Length': Buffer.byteLength(body),
    'Accept': 'text/event-stream',
  },
};
const client = parsed.protocol === 'https:' ? https : http;

let stamped = false;
function stampOnce() {
  if (!stamped) { fs.writeSync(1, `[ts:${Date.now()}]\n`); stamped = true; }
}

let assistantBuf = '';
let midline = false;

const req = client.request(opts, (resp) => {
  if (resp.statusCode !== 200) {
    let errBody = '';
    resp.on('data', (chunk) => { errBody += chunk.toString('utf8'); });
    resp.on('end', () => {
      writeErr(`venice: HTTP ${resp.statusCode}: ${errBody.slice(0, 500).replace(/\n/g, ' ')}`);
    });
    return;
  }
  let sseBuf = '';
  resp.setEncoding('utf8');
  resp.on('data', (chunk) => {
    sseBuf += chunk;
    // SSE frames are separated by \n\n; within a frame we look for `data:` lines.
    let idx;
    while ((idx = sseBuf.indexOf('\n')) !== -1) {
      const line = sseBuf.slice(0, idx);
      sseBuf = sseBuf.slice(idx + 1);
      const trimmed = line.trim();
      if (!trimmed.startsWith('data:')) continue;
      const payload = trimmed.slice(5).trim();
      if (payload === '[DONE]') continue;
      let ev;
      try { ev = JSON.parse(payload); } catch { continue; }
      const choices = ev.choices;
      if (!Array.isArray(choices) || choices.length === 0) continue;
      const delta = choices[0].delta;
      if (!delta || typeof delta.content !== 'string' || delta.content.length === 0) continue;
      stampOnce();
      // Write delta text directly to the log. tailLog treats unterminated
      // buffers as `stream` events and \n-terminated lines as `done`,
      // matching how the Claude path streams partial content.
      fs.writeSync(1, delta.content);
      assistantBuf += delta.content;
      midline = !delta.content.endsWith('\n');
    }
  });
  resp.on('end', () => {
    // Terminating newline so the final partial line flushes as a `done`
    // event in the TUI, and so the next turn's `[ts:N]` marker starts on
    // its own line.
    if (midline) fs.writeSync(1, '\n');
    if (assistantBuf.length > 0) {
      history.push({ role: 'user', content: USER_MSG });
      history.push({ role: 'assistant', content: assistantBuf });
      try {
        fs.writeFileSync(HISTORY, JSON.stringify(history));
      } catch (e) {
        writeErr(`venice: history write failed: ${e.message}`);
      }
    }
  });
});

req.on('error', (e) => {
  writeErr(`venice: request error: ${e.message}`);
});
req.write(body);
req.end();
