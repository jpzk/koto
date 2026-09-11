#!/usr/bin/env node
// Venice provider runner with tool use (bash + file).
//
// Inputs from env (set by entrypoint.sh):
//   MSG_B64       base64-encoded user message (already augmented by the
//                 daemon with the <koto-context> block).
//   SP_B64        base64-encoded system prompt (composeSystemPrompt output);
//                 empty if no global/per-group prompt is configured.
//   VENICE_MODEL  model name (default applied in entrypoint.sh).
//
// History lives in /workspace/.cs/venice-history.json. Venice's chat API is
// stateless so we replay the whole transcript on every turn, including any
// tool_calls / tool messages from prior turns. /clear (handled in daemon's
// clearCmd) wipes the file.
//
// Output framing matches the Claude path so daemon.go's tailLog parses both
// providers identically:
//   [ts:N]\n                              — stamped once before first text byte
//   <delta text>                          — written verbatim
//   [[tool]] <name> <json args>\n         — per tool call announcement
//   [[tool_out_begin]]\n<result>\n[[tool_out_end]] <bytes>\n
//                                         — per tool result (framed block)
//   [[err]] venice: ...\n                 — errors surface in red in TUI
//
// Tool loop: after each assistant turn, if the response contained
// `tool_calls`, we execute each, append the results as role:tool messages,
// and re-call Venice. Loops up to TOOL_BUDGET (default 25) per user message
// to prevent runaway. Trust model unchanged — bash runs as `node` (uid 1000)
// inside the sidecar container, same blast radius as the claude path's bash.

const fs = require('fs');
const http = require('http');
const https = require('https');
const url = require('url');
const path = require('path');
const { spawn } = require('child_process');

// KOTO_VH_FILE is set by entrypoint.sh: the default chat session keeps the
// historical filename, named sessions each get venice-history-<name>.json so
// their transcripts stay independent (per-session /clear removes one file).
const HISTORY = process.env.KOTO_VH_FILE || '/workspace/.cs/venice-history.json';
const BASE = process.env.ANTHROPIC_BASE_URL;
// VENICE_MODEL is always set by entrypoint.sh (config model, else the daemon's
// KOTO_DEFAULT_VENICE_MODEL). This literal is a last-resort fallback only;
// keep it aligned with defaultVeniceModel in daemon.go.
const MODEL = process.env.VENICE_MODEL || 'kimi-k2.5';
const TOOL_BUDGET = 25;
const BASH_TIMEOUT_MS = 30_000;
const OUTPUT_CAP_BYTES = 1_000_000;
const FILE_READ_CAP_BYTES = 1_000_000;

function decodeB64(s) {
  if (!s) return '';
  try { return Buffer.from(s, 'base64').toString('utf8'); } catch { return ''; }
}
const USER_MSG = decodeB64(process.env.MSG_B64);
const SYS_PROMPT = decodeB64(process.env.SP_B64);
// Read once, then removed from this process's environment — every child
// inherits process.env, and the bash tool runs attacker-influenceable
// workspace code (a repo's build script, an npm postinstall) in /workspace. It
// could read the turn's prompt and system prompt straight out of its own
// environment, without the model ever choosing to reveal them, and exfiltrate
// them over whatever egress the group's network profile allows (audit M58).
// Base64 is an encoding, not a confidentiality control. Deleting here rather
// than filtering at each spawn keeps it true for every child, including ones
// added later.
delete process.env.MSG_B64;
delete process.env.SP_B64;

// VENICE_ONESHOT: sub-agent mode (driven by cs-subagent). The streaming deltas
// and tool framing are progress, not the return value, so they go to stderr;
// only the final answer text is written to stdout (see the done branch). History
// is ephemeral (saveHistory/loadHistory are skipped) so a sub-call never touches
// the group's main /workspace/.cs/venice-history.json thread.
const ONESHOT = !!process.env.VENICE_ONESHOT;
// KOTO_EVENTS: the chat-turn mode under fc-agent (fcguest/turn.go). Instead of
// [[marker]] text, every event is one JSON line on stdout —
//   {"ev":"text","text":..} {"ev":"tool","name":..,"input":..}
//   {"ev":"tool_out","text":..} {"ev":"err","text":..}
// — which fc-agent turns into typed TurnFrames; the host renders the marker
// text. Marker text output survives only for VENICE_ONESHOT (cs-subagent), where
// it lands in a job's out file.
const EVENTS = !!process.env.KOTO_EVENTS && !ONESHOT;
function emit(ev) { try { fs.writeSync(1, JSON.stringify(ev) + '\n'); } catch {} }
function writeOut(s) {
  if (EVENTS) { if (s.length) emit({ ev: 'text', text: s }); return; }
  try { fs.writeSync(ONESHOT ? 2 : 1, s); } catch {}
}
function writeFinal(s) { try { fs.writeSync(1, s); } catch {} }
function writeErr(s) { if (EVENTS) emit({ ev: 'err', text: s }); else writeOut(`[[err]] ${s}\n`); }

if (!BASE) { writeErr('venice: ANTHROPIC_BASE_URL not set'); process.exit(ONESHOT ? 1 : 0); }
if (!USER_MSG) { writeErr('venice: empty MSG_B64'); process.exit(ONESHOT ? 1 : 0); }

// ---- log framing helpers --------------------------------------------------

let stamped = false;
let midline = false;
function stampOnce() {
  if (EVENTS) return; // the host stamps the turn
  if (!stamped) { writeOut(`[ts:${Date.now()}]\n`); stamped = true; }
}
function breakLine() {
  if (EVENTS) return; // the host owns line state
  if (midline) { writeOut('\n'); midline = false; }
}
// Same escape rule as stream_filter.js's escapeBody — body lines that look
// like our own block-close marker get prefixed with `\` so the daemon's log
// tailer can't be tricked into closing the frame early from inside payload.
function escapeBody(s) {
  return s.replace(/^(\[\[tool_out_end\]\] )/gm, '\\$1');
}
function emitToolCallHeader(name, argsJson) {
  if (EVENTS) { emit({ ev: 'tool', name, input: argsJson }); return; }
  stampOnce();
  breakLine();
  writeOut(`[[tool]] ${name} ${argsJson}\n`);
}
function emitToolOut(body) {
  if (EVENTS) { emit({ ev: 'tool_out', text: body }); return; }
  const safe = escapeBody(body);
  const bytes = Buffer.byteLength(body, 'utf8');
  writeOut('[[tool_out_begin]]\n');
  if (safe.length) writeOut(safe.endsWith('\n') ? safe : safe + '\n');
  writeOut(`[[tool_out_end]] ${bytes}\n`);
}

// ---- tool implementations -------------------------------------------------
//
// Each tool returns a plain object. We JSON.stringify it for the role:tool
// message content (OpenAI/Venice expect a string) and pretty-print it for the
// [[tool_out_*]] log block so the human-facing rendering stays readable.

function execBash(command) {
  return new Promise((resolve) => {
    if (typeof command !== 'string' || !command) {
      resolve({ error: 'bash: command must be a non-empty string' });
      return;
    }
    const proc = spawn('bash', ['-lc', command], {
      cwd: '/workspace',
      env: process.env,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let out = Buffer.alloc(0);
    let truncated = false;
    let timedOut = false;
    const collect = (chunk) => {
      if (truncated) return;
      if (out.length + chunk.length > OUTPUT_CAP_BYTES) {
        const room = Math.max(0, OUTPUT_CAP_BYTES - out.length);
        if (room > 0) out = Buffer.concat([out, chunk.subarray(0, room)]);
        truncated = true;
        return;
      }
      out = Buffer.concat([out, chunk]);
    };
    proc.stdout.on('data', collect);
    proc.stderr.on('data', collect);
    const killTimer = setTimeout(() => {
      timedOut = true;
      try { proc.kill('SIGTERM'); } catch {}
      setTimeout(() => { try { proc.kill('SIGKILL'); } catch {} }, 2000);
    }, BASH_TIMEOUT_MS);
    proc.on('close', (code, signal) => {
      clearTimeout(killTimer);
      const result = { exit_code: code, output: out.toString('utf8') };
      if (signal) result.signal = signal;
      if (truncated) result.truncated = true;
      if (timedOut) result.timed_out_after_ms = BASH_TIMEOUT_MS;
      resolve(result);
    });
    proc.on('error', (e) => {
      clearTimeout(killTimer);
      resolve({ error: `bash spawn failed: ${e.message}` });
    });
  });
}

function execFile(args) {
  const op = args && args.op;
  const p = args && args.path;
  if (!op || !p) return { error: 'file: op and path are required' };
  try {
    switch (op) {
      case 'read': {
        const st = fs.statSync(p);
        if (st.isDirectory()) return { error: `file read: ${p} is a directory` };
        const buf = fs.readFileSync(p);
        if (buf.length > FILE_READ_CAP_BYTES) {
          return {
            content: buf.subarray(0, FILE_READ_CAP_BYTES).toString('utf8'),
            truncated: true,
            total_bytes: buf.length,
          };
        }
        return { content: buf.toString('utf8'), bytes: buf.length };
      }
      case 'write': {
        const content = typeof args.content === 'string' ? args.content : '';
        fs.mkdirSync(path.dirname(p), { recursive: true });
        fs.writeFileSync(p, content);
        return { ok: true, bytes_written: Buffer.byteLength(content, 'utf8') };
      }
      case 'edit': {
        const oldS = args.old;
        const newS = args.new;
        if (typeof oldS !== 'string' || typeof newS !== 'string') {
          return { error: 'file edit: old and new must be strings' };
        }
        const orig = fs.readFileSync(p, 'utf8');
        const first = orig.indexOf(oldS);
        if (first === -1) return { error: `file edit: old string not found in ${p}` };
        if (orig.indexOf(oldS, first + oldS.length) !== -1) {
          return { error: `file edit: old string is not unique in ${p}` };
        }
        const next = orig.slice(0, first) + newS + orig.slice(first + oldS.length);
        fs.writeFileSync(p, next);
        return { ok: true };
      }
      default:
        return { error: `file: unknown op "${op}"` };
    }
  } catch (e) {
    return { error: `file ${op} ${p}: ${e.message}` };
  }
}

async function executeToolCall(call) {
  // call.function.arguments is a JSON string per OpenAI spec; may be malformed
  // if the model emitted bad JSON or streaming was cut short.
  let args = {};
  try {
    args = call.function && call.function.arguments
      ? JSON.parse(call.function.arguments)
      : {};
  } catch (e) {
    return { error: `tool arg parse failed: ${e.message}`, raw: call.function && call.function.arguments };
  }
  const name = call.function && call.function.name;
  switch (name) {
    case 'bash': return await execBash(args.command);
    case 'file': return execFile(args);
    default:    return { error: `unknown tool: ${name}` };
  }
}

// ---- tool schema (OpenAI shape) -------------------------------------------

const TOOLS = [
  {
    type: 'function',
    function: {
      name: 'bash',
      description:
        'Run a shell command via bash -lc in /workspace. Returns exit_code and combined stdout+stderr. ' +
        `Output is capped at ${OUTPUT_CAP_BYTES} bytes (truncated flag set if hit). ` +
        `Killed after ${BASH_TIMEOUT_MS / 1000}s. Use for inspection, builds, git, etc.`,
      parameters: {
        type: 'object',
        properties: {
          command: { type: 'string', description: 'Shell command to execute.' },
        },
        required: ['command'],
      },
    },
  },
  {
    type: 'function',
    function: {
      name: 'file',
      description:
        'Read, write, or edit a file in the sidecar filesystem. ' +
        'read: returns text content (capped at 1MB). ' +
        'write: overwrites the file, creating parent dirs as needed. ' +
        'edit: literal-string replacement; old must appear exactly once in the file.',
      parameters: {
        type: 'object',
        properties: {
          op:      { type: 'string', enum: ['read', 'write', 'edit'] },
          path:    { type: 'string', description: 'Absolute or workspace-relative path.' },
          content: { type: 'string', description: 'Full file content for op=write.' },
          old:     { type: 'string', description: 'Literal string to replace for op=edit.' },
          new:     { type: 'string', description: 'Replacement string for op=edit.' },
        },
        required: ['op', 'path'],
      },
    },
  },
];

// ---- history --------------------------------------------------------------

function loadHistory() {
  try {
    const raw = fs.readFileSync(HISTORY, 'utf8');
    const parsed = JSON.parse(raw);
    // Trimmed on load too: a transcript written before the cap existed, or by
    // an older sidecar, must not be replayed whole into the request.
    return Array.isArray(parsed) ? trimHistory(parsed) : [];
  } catch { return []; }
}

// HISTORY_MAX_BYTES bounds the serialized transcript.
//
// Venice's chat API is stateless, so the whole transcript is replayed on every
// request — an unbounded one grows the session workspace, grows every request,
// and eventually exceeds the proxy's 64 MiB body cap, at which point that
// session can make no further Venice turns at all until someone clears it
// (audit M90). The failure is silent up to that point and then total.
//
// 4 MiB is far more context than any Venice model accepts, so trimming to it
// costs nothing the provider would have used.
const HISTORY_MAX_BYTES = 4 * 1024 * 1024;

// trimHistory drops the OLDEST entries until the transcript fits, always
// keeping the most recent ones — the model's own context window works the same
// way, and a turn is far more likely to need what just happened.
//
// Whole entries, never partial: a truncated tool_calls entry without its
// matching role:"tool" reply is a malformed conversation the API rejects, so a
// size cap that split one would turn a large session into a broken one.
function trimHistory(h) {
  let out = h;
  while (out.length > 1 && Buffer.byteLength(JSON.stringify(out)) > HISTORY_MAX_BYTES) {
    // Drop from the front, and keep dropping past any orphaned tool replies so
    // the surviving head is a coherent conversation.
    out = out.slice(1);
    while (out.length > 1 && out[0] && out[0].role === 'tool') out = out.slice(1);
  }
  return out;
}

function saveHistory(h) {
  if (ONESHOT) return; // sub-agent history is ephemeral — never persist it
  const trimmed = trimHistory(h);
  if (trimmed.length < h.length) {
    writeErr(`venice: transcript trimmed to the newest ${trimmed.length} of ${h.length} messages (${HISTORY_MAX_BYTES >> 20} MiB cap)`);
    h.length = 0;
    h.push(...trimmed); // keep the caller's in-memory copy in step
  }
  try { fs.writeFileSync(HISTORY, JSON.stringify(trimmed)); }
  catch (e) { writeErr(`venice: history write failed: ${e.message}`); }
}

// ---- one turn against Venice ---------------------------------------------
//
// Returns { text, toolCalls, error }. text is anything written via
// delta.content; toolCalls is an array of accumulated tool_calls in the
// OpenAI shape (with id, function.name, function.arguments as a string).

function streamTurn(messages) {
  return new Promise((resolve) => {
    // stream_options.include_usage is the OpenAI-compatible switch that makes
    // Venice emit the usage block (incl. prompt_tokens_details.cached_tokens)
    // on the terminal stream chunk. Venice currently sends usage without it,
    // but that's undocumented for the streaming path — set it explicitly so the
    // proxy's per-request metrics don't silently go empty on a Venice update.
    const body = JSON.stringify({ model: MODEL, messages, tools: TOOLS, stream: true, stream_options: { include_usage: true } });
    const parsed = url.parse(BASE + '/api/v1/chat/completions');
    const opts = {
      protocol: parsed.protocol,
      hostname: parsed.hostname,
      port: parsed.port,
      path: parsed.path,
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'Authorization': 'Bearer proxied',
        'Content-Length': Buffer.byteLength(body),
        'Accept': 'text/event-stream',
      },
    };
    const client = parsed.protocol === 'https:' ? https : http;

    const req = client.request(opts, (resp) => {
      if (resp.statusCode !== 200) {
        let errBody = '';
        resp.on('data', (c) => { errBody += c.toString('utf8'); });
        resp.on('end', () => {
          resolve({ error: `HTTP ${resp.statusCode}: ${errBody.slice(0, 500).replace(/\n/g, ' ')}` });
        });
        return;
      }
      let sseBuf = '';
      let text = '';
      const toolCallsByIdx = new Map();
      resp.setEncoding('utf8');
      resp.on('data', (chunk) => {
        sseBuf += chunk;
        let idx;
        while ((idx = sseBuf.indexOf('\n')) !== -1) {
          const line = sseBuf.slice(0, idx);
          sseBuf = sseBuf.slice(idx + 1);
          const t = line.trim();
          if (!t.startsWith('data:')) continue;
          const payload = t.slice(5).trim();
          if (payload === '[DONE]') continue;
          let ev;
          try { ev = JSON.parse(payload); } catch { continue; }
          const choices = ev.choices;
          if (!Array.isArray(choices) || choices.length === 0) continue;
          const delta = choices[0].delta;
          if (!delta) continue;
          if (typeof delta.content === 'string' && delta.content.length > 0) {
            stampOnce();
            writeOut(delta.content);
            text += delta.content;
            midline = !delta.content.endsWith('\n');
          }
          if (Array.isArray(delta.tool_calls)) {
            for (const tc of delta.tool_calls) {
              const i = typeof tc.index === 'number' ? tc.index : 0;
              let acc = toolCallsByIdx.get(i);
              if (!acc) {
                acc = { id: '', type: 'function', function: { name: '', arguments: '' } };
                toolCallsByIdx.set(i, acc);
              }
              if (tc.id) acc.id = tc.id;
              if (tc.function) {
                if (tc.function.name) acc.function.name = tc.function.name;
                if (tc.function.arguments) acc.function.arguments += tc.function.arguments;
              }
            }
          }
        }
      });
      resp.on('end', () => {
        const toolCalls = [...toolCallsByIdx.entries()]
          .sort((a, b) => a[0] - b[0])
          .map(([, v]) => v)
          .filter((c) => c.function && c.function.name);
        resolve({ text, toolCalls });
      });
      resp.on('error', (e) => resolve({ error: `response error: ${e.message}` }));
    });
    req.on('error', (e) => resolve({ error: `request error: ${e.message}` }));
    req.write(body);
    req.end();
  });
}

// ---- main loop ------------------------------------------------------------

(async () => {
  const history = ONESHOT ? [] : loadHistory();
  const messages = [];
  if (SYS_PROMPT) messages.push({ role: 'system', content: SYS_PROMPT });
  for (const m of history) messages.push(m);
  messages.push({ role: 'user', content: USER_MSG });

  // Persist the user turn immediately so /clear-after-failure still scrubs
  // it; tool/assistant turns are appended after each successful round.
  history.push({ role: 'user', content: USER_MSG });

  for (let iter = 0; iter < TOOL_BUDGET; iter++) {
    const turn = await streamTurn(messages);
    if (turn.error) {
      writeErr(`venice: ${turn.error}`);
      if (ONESHOT) process.exitCode = 1;
      saveHistory(history);
      return;
    }
    // Build the assistant message in OpenAI shape — content may be null when
    // the response is tool-calls only; some Venice variants reject null and
    // want '' instead, so use empty string.
    const assistantMsg = { role: 'assistant', content: turn.text || '' };
    if (turn.toolCalls.length > 0) assistantMsg.tool_calls = turn.toolCalls;
    messages.push(assistantMsg);
    history.push(assistantMsg);

    if (turn.toolCalls.length === 0) {
      // Pure text response — done.
      if (ONESHOT) {
        // The final answer is this turn's text; emit it (only) to stdout as the
        // sub-agent's return value, with a trailing newline.
        const ans = turn.text || '';
        writeFinal(ans.endsWith('\n') ? ans : ans + '\n');
      } else if (midline) {
        // Flush trailing newline so the last partial line surfaces as a `done`
        // event in the TUI.
        writeOut('\n');
      }
      saveHistory(history);
      return;
    }

    // Execute each tool call sequentially (parallel would muddle the log
    // ordering). Append role:tool messages with the JSON-stringified result
    // so Venice can ingest them on the next turn.
    for (const call of turn.toolCalls) {
      emitToolCallHeader(call.function.name, call.function.arguments || '{}');
      const result = await executeToolCall(call);
      const resultStr = JSON.stringify(result);
      // Human-readable rendering in the log: prefer raw bash output / file
      // content directly under the result frame; everything else goes as
      // pretty JSON.
      let rendered;
      if (typeof result.output === 'string' && result.exit_code !== undefined) {
        rendered = `exit=${result.exit_code}${result.truncated ? ' (truncated)' : ''}${result.timed_out_after_ms ? ` timed_out_after_ms=${result.timed_out_after_ms}` : ''}\n${result.output}`;
      } else if (typeof result.content === 'string') {
        rendered = result.content;
      } else {
        rendered = JSON.stringify(result, null, 2);
      }
      emitToolOut(rendered);
      const toolMsg = {
        role: 'tool',
        tool_call_id: call.id,
        content: resultStr,
      };
      messages.push(toolMsg);
      history.push(toolMsg);
    }
    // Loop back: re-call Venice with the appended tool results.
  }

  writeErr(`venice: tool-call budget exhausted (${TOOL_BUDGET}); stopping`);
  if (ONESHOT) process.exitCode = 1;
  saveHistory(history);
})();
