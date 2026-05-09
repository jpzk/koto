// FORCE_COLOR=3 is set in tui.Dockerfile ENV — must be set in the env BEFORE
// bun starts because chalk's color detection runs at import time, before any
// JS in this file executes.

import React, { useEffect, useState, useRef } from 'react';
import { render, Box, Text, useStdout, useInput, useApp } from 'ink';
import TextInput from 'ink-text-input';
import { marked } from 'marked';
import { markedTerminal } from 'marked-terminal';
import { highlight } from 'cli-highlight';
import * as net from 'node:net';
import { PLUGINS, PLUGIN_BY_NAME, type Plugin, type PluginCtx, type Event as PluginEvent } from './plugins';

// `\`\`\`ansi`-fenced blocks: claude can emit them when it wants to write raw
// ANSI escape codes that should pass through verbatim (e.g. colored diff
// output). Default markdown rendering would escape/strip the codes — this
// extension forwards them as-is.
marked.use({
  extensions: [{
    name: 'ansiBlock',
    level: 'block',
    start(src: string) { const i = src.indexOf('```ansi'); return i < 0 ? undefined : i; },
    tokenizer(src: string) {
      const m = /^```ansi\r?\n([\s\S]*?)\r?\n```/.exec(src);
      if (!m) return undefined;
      return { type: 'ansiBlock', raw: m[0], text: m[1]! };
    },
    renderer(token: { text: string }) { return token.text + '\n'; },
  }],
} as any);

// Markdown → ANSI for response blocks. reflowText:false lets Ink's wrap=wrap
// handle terminal-width wrapping; marked-terminal does code blocks (with
// cli-highlight syntax colors), lists, headings, bold/italic.
marked.use(markedTerminal({
  reflowText: false,
  width: 1_000_000,
  tab: 2,
  code: (code: string, lang?: string) => {
    try { return highlight(code, { language: lang || 'plaintext', ignoreIllegals: true }); }
    catch { return code; }
  },
}) as any);

function md(text: string): string {
  try {
    const out = marked.parse(text, { async: false }) as string;
    return out.replace(/\n+$/, '');
  } catch { return text; }
}

process.on('SIGINT',  () => process.exit(130));
process.on('SIGTERM', () => process.exit(143));

const SOCK = process.env.SOCK_PATH || '/sock';
const MAX_LINES = 500;
// Approximate model context window for the ctx% display. Most current
// Claude models are 200k; Opus 4.7 in 1M-context mode would render at ~5x
// less than its real headroom — acceptable as a default. Override with
// CTX_WINDOW env var if your group runs a wider-context model.
const CTX_WINDOW = parseInt(process.env.CTX_WINDOW || '200000', 10);
const SPINNER = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];

type Groups = Record<string, { port: number; running: boolean }>;
type LogLine = { kind: 'prompt' | 'response' | 'sys' | 'err'; group: string; text: string; ts?: number };
type Event = { event: 'prompt' | 'stream' | 'done'; group: string; msg?: string; text?: string; ts?: number; historical?: boolean };

function humanTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000)     return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

// Pick a foreground color for a 0..1 utilization value. Below 50% is calm
// cyan, 50-80% is yellow (caution), >=80% is red (danger). Used for both
// 5h and 7d budget segments and for context-size warning.
function pctColor(frac: number): 'cyan' | 'yellow' | 'red' {
  if (frac >= 0.80) return 'red';
  if (frac >= 0.50) return 'yellow';
  return 'cyan';
}

function fmtTime(ts: number | undefined): string {
  if (!ts) return '';
  const d = new Date(ts * 1000);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  return `${hh}:${mm}`;
}

function call(cmd: string, extra: Record<string, unknown> = {}): Promise<any> {
  return new Promise((resolve, reject) => {
    const c = net.createConnection(SOCK);
    let buf = '';
    c.on('connect', () => c.write(JSON.stringify({ cmd, ...extra }) + '\n'));
    c.on('data', d => {
      buf += d.toString();
      const nl = buf.indexOf('\n');
      if (nl >= 0) {
        try { resolve(JSON.parse(buf.slice(0, nl))); }
        catch (e) { reject(e); }
        c.end();
      }
    });
    c.on('error', reject);
  });
}

function subscribe(group: string, onEvent: (e: Event) => void, onErr: (msg: string) => void, onClose: () => void): net.Socket {
  const c = net.createConnection(SOCK);
  let buf = '';
  let acked = false;
  c.on('connect', () => c.write(JSON.stringify({ cmd: 'subscribe', group }) + '\n'));
  c.on('data', d => {
    buf += d.toString();
    let nl: number;
    while ((nl = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, nl); buf = buf.slice(nl + 1);
      if (!line.trim()) continue;
      try {
        const m = JSON.parse(line);
        if (!acked && m.ok) { acked = true; continue; }
        if (m.event) onEvent(m as Event);
      } catch (e: any) { onErr(`parse: ${e.message}`); }
    }
  });
  c.on('error', e => onErr(`sub ${group}: ${e.message}`));
  c.on('close', onClose);
  return c;
}

// Plugin runtime: builds an event queue + AbortController + the helper
// closures listed in PluginCtx, then awaits plugin.run(ctx). Returns a
// handle the App stores in activePluginRef so onEvent can push events
// into the queue and Ctrl+C can abort.
type PluginHandle = {
  name: string;
  abort: AbortController;
  push: (ev: PluginEvent) => void;
  done: Promise<void>;
};

function runPlugin(
  plugin: Plugin,
  args: string,
  group: string,
  helpers: {
    log: (text: string, kind?: 'sys' | 'err', g?: string) => void;
    callDaemon: (cmd: string, extra?: Record<string, unknown>) => Promise<any>;
  },
): PluginHandle {
  const abort = new AbortController();
  const buf: PluginEvent[] = [];
  let waiter: ((ev: PluginEvent | null) => void) | null = null;

  const push = (ev: PluginEvent) => {
    if (ev.group !== group) return;
    if (waiter) { const w = waiter; waiter = null; w(ev); }
    else buf.push(ev);
  };

  const nextEvent = (timeoutMs: number): Promise<PluginEvent | null> =>
    new Promise(resolve => {
      if (abort.signal.aborted) return resolve(null);
      if (buf.length > 0) return resolve(buf.shift()!);
      let settled = false;
      const finish = (v: PluginEvent | null) => { if (settled) return; settled = true; clearTimeout(t); abort.signal.removeEventListener('abort', onAbort); resolve(v); };
      const onAbort = () => finish(null);
      const t = setTimeout(() => finish(null), timeoutMs);
      abort.signal.addEventListener('abort', onAbort);
      waiter = (ev) => finish(ev);
    });

  const sleep = (ms: number): Promise<void> =>
    new Promise(resolve => {
      if (abort.signal.aborted) return resolve();
      const t = setTimeout(() => { abort.signal.removeEventListener('abort', onAbort); resolve(); }, ms);
      const onAbort = () => { clearTimeout(t); resolve(); };
      abort.signal.addEventListener('abort', onAbort);
    });

  const ctx: PluginCtx = {
    group,
    args,
    signal: abort.signal,
    call: helpers.callDaemon,
    send: async (msg: string) => { await helpers.callDaemon('send', { group, msg }); },
    metrics: async () => {
      const r = await helpers.callDaemon('metrics', { group });
      return r?.metric ?? null;
    },
    log: (text: string, kind: 'sys' | 'err' = 'sys') => helpers.log(text, kind, group),
    nextEvent,
    sleep,
  };

  const done = (async () => {
    try { await plugin.run(ctx); }
    catch (e: any) {
      if (!abort.signal.aborted) helpers.log(`plugin /${plugin.name} crashed: ${e?.message || e}`, 'err', group);
    }
  })();

  return { name: plugin.name, abort, push, done };
}

const App = () => {
  const { exit } = useApp();
  const { stdout } = useStdout();
  const [size, setSize] = useState({ rows: stdout.rows, cols: stdout.columns });
  const [groups, setGroups] = useState<Groups>({});
  const [cur, setCur] = useState('main');
  const [lines, setLines] = useState<LogLine[]>([]);
  const [streamBuf, setStreamBuf] = useState<Record<string, string>>({});
  const [input, setInput] = useState('');
  const [tick, setTick] = useState(0);
  const [scroll, setScroll] = useState(0); // lines above bottom; 0 = pinned to bottom
  const [connected, setConnected] = useState(true);
  const subsRef = useRef<Map<string, net.Socket>>(new Map());
  const reconnectingRef = useRef(false);
  const activePluginRef = useRef<PluginHandle | null>(null);
  const [pluginName, setPluginName] = useState<string | null>(null);
  const [metric, setMetric] = useState<any>(null);             // cur-group metric (for ctx)
  const [globalMetric, setGlobalMetric] = useState<any>(null); // newest across groups (for budget)

  useEffect(() => {
    const onResize = () => setSize({ rows: stdout.rows, cols: stdout.columns });
    stdout.on('resize', onResize);
    return () => { stdout.off('resize', onResize); };
  }, [stdout]);

  useEffect(() => {
    const id = setInterval(() => setTick(t => t + 1), 80);
    return () => clearInterval(id);
  }, []);

  // Poll metrics every few seconds. The daemon returns BOTH the active
  // group's latest metric (for context size) and the globally-newest
  // metric across all groups (for the account-wide 5h/7d rate-limit,
  // which is shared across groups and visible even when cur is stopped).
  useEffect(() => {
    if (!connected) return;
    let alive = true;
    const fetchMetric = async () => {
      try {
        const r = await call('metrics', { group: cur });
        if (!alive || !r?.ok) return;
        setMetric(r.metric ?? null);
        setGlobalMetric(r.global_metric ?? null);
      } catch {}
    };
    fetchMetric();
    const id = setInterval(fetchMetric, 5000);
    return () => { alive = false; clearInterval(id); };
  }, [cur, connected]);

  const addLine = (l: LogLine) => setLines(ls => {
    const next = ls.concat(l);
    return next.length > MAX_LINES ? next.slice(-MAX_LINES) : next;
  });

  const onEvent = (ev: Event) => {
    // Trust ev.ts in both live and historical cases — the daemon now embeds
    // [ts:N] markers in .cs/log so historical events carry accurate timestamps
    // captured when the prompt or response actually happened.
    const ts = ev.ts;
    if (ev.event === 'prompt') {
      setStreamBuf(b => {
        const cur = b[ev.group];
        if (cur) addLine({ kind: 'response', group: ev.group, text: cur });
        const { [ev.group]: _, ...rest } = b;
        return rest;
      });
      addLine({ kind: 'prompt', group: ev.group, text: ev.msg || '', ts });
    } else if (ev.event === 'stream') {
      setStreamBuf(b => ({ ...b, [ev.group]: ev.text || '' }));
    } else if (ev.event === 'done') {
      setStreamBuf(b => { const { [ev.group]: _, ...rest } = b; return rest; });
      if (ev.text) addLine({ kind: 'response', group: ev.group, text: ev.text, ts });
    }
    // Forward live events to the active plugin (if its group matches).
    // historical=true events from history replay are not interesting to a
    // newly-started plugin observing real-time activity.
    if (!ev.historical) activePluginRef.current?.push(ev as PluginEvent);
  };

  const scheduleReconnect = () => {
    if (reconnectingRef.current) return;
    reconnectingRef.current = true;
    setConnected(false);
    // Tear down stale subscribe sockets and forget already-known groups so the
    // post-reconnect refresh re-fetches history and re-subscribes from scratch.
    for (const s of subsRef.current.values()) { try { s.destroy(); } catch {} }
    subsRef.current.clear();

    let attempt = 0;
    const tryReconnect = async () => {
      attempt += 1;
      try {
        await call('list');
        reconnectingRef.current = false;
        setConnected(true);
        addLine({ kind: 'sys', group: '', text: `reconnected to daemon` });
        refresh();
      } catch {
        const delay = Math.min(5000, 250 * 2 ** Math.min(attempt, 5));
        setTimeout(tryReconnect, delay);
      }
    };
    setTimeout(tryReconnect, 200);
  };

  const refresh = async () => {
    try {
      const r = await call('list');
      const gs: Groups = r.groups || {};
      setGroups(gs);
      if (!connected) setConnected(true);
      for (const g of Object.keys(gs)) {
        if (subsRef.current.has(g)) continue;
        // 1. fetch + bulk-replay history (one setLines for the whole batch)
        try {
          const h = await call('history', { group: g });
          if (h.ok && Array.isArray(h.events) && h.events.length > 0) {
            const batch: LogLine[] = [];
            for (const ev of h.events as Event[]) {
              if (ev.event === 'prompt')      batch.push({ kind: 'prompt',   group: g, text: ev.msg || '', ts: ev.ts });
              else if (ev.event === 'done' && ev.text) batch.push({ kind: 'response', group: g, text: ev.text, ts: ev.ts });
            }
            if (batch.length) setLines(prev => {
              const merged = prev.concat(batch);
              return merged.length > MAX_LINES ? merged.slice(-MAX_LINES) : merged;
            });
          }
        } catch (e: any) {
          addLine({ kind: 'err', group: g, text: `history: ${e.message || e}` });
        }
        // 2. subscribe for live events going forward
        subsRef.current.set(g, subscribe(g, onEvent,
          msg => addLine({ kind: 'err', group: g, text: msg }),
          scheduleReconnect));
      }
    } catch (e: any) {
      addLine({ kind: 'err', group: '', text: `daemon: ${e.message || e}` });
      scheduleReconnect();
    }
  };

  useEffect(() => {
    refresh();
    const cleanup = () => { for (const s of subsRef.current.values()) s.end(); };
    process.on('exit', cleanup);
    return () => { process.off('exit', cleanup); cleanup(); };
  }, []);

  const onSubmit = async (v: string) => {
    setInput('');
    const t = v.trim();
    if (!t) return;
    if (t.startsWith('/new ')) {
      const g = t.slice(5).trim();
      try {
        const r = await call('spawn', { group: g });
        if (r.ok) addLine({ kind: 'sys', group: '', text: `spawned ${g}` });
        else addLine({ kind: 'err', group: '', text: `spawn ${g}: ${r.error}` });
      } catch (e: any) {
        addLine({ kind: 'err', group: '', text: `spawn ${g}: ${e.message || e}` });
      }
      refresh();
    } else if (t.startsWith('/sw ')) {
      // No validation — daemon auto-spawns on send if group isn't running yet.
      setCur(t.slice(4).trim());
      refresh();
    } else if (t === '/ls') {
      refresh();
    } else if (t === '/clear') {
      // Wipe both claude's session and our log for the active group.
      // Next message starts a fresh conversation.
      try {
        const r = await call('clear', { group: cur });
        if (r.ok) {
          // Drop any rendered lines for this group; daemon's tail thread
          // will resume from byte 0 of the now-empty log on next write.
          setLines(prev => prev.filter(l => l.group !== cur));
          setStreamBuf(b => { const { [cur]: _, ...rest } = b; return rest; });
          addLine({ kind: 'sys', group: cur, text: `cleared context for ${cur}` });
        } else {
          addLine({ kind: 'err', group: cur, text: `clear: ${r.error}` });
        }
      } catch (e: any) {
        addLine({ kind: 'err', group: cur, text: `clear: ${e.message || e}` });
      }
    } else if (t === '/stop-plugin' || t.startsWith('/stop-plugin ')) {
      // Hard-stop a running plugin by name. Name must match the active
      // plugin — protects against killing the wrong thing if the user
      // mistypes or comes back to the TUI after the plugin already ended.
      const target = t.length > 12 ? t.slice(13).trim() : '';
      const h = activePluginRef.current;
      if (!target) {
        addLine({ kind: 'err', group: cur, text: 'usage: /stop-plugin <name>' });
      } else if (!h) {
        addLine({ kind: 'sys', group: cur, text: 'no plugin running' });
      } else if (h.name !== target) {
        addLine({ kind: 'err', group: cur, text: `active plugin is /${h.name}, not /${target}` });
      } else {
        h.abort.abort();
        addLine({ kind: 'sys', group: '', text: `stopped /${h.name}` });
      }
    } else if (t.startsWith('/') && !t.startsWith('/new ') && !t.startsWith('/sw ')
               && t !== '/ls' && t !== '/clear'
               && !t.startsWith('/stop-plugin') && t !== '/config' && !t.startsWith('/config ')) {
      // Plugin dispatch — single-running slot.
      const sp = t.indexOf(' ');
      const name = (sp < 0 ? t.slice(1) : t.slice(1, sp));
      const args = sp < 0 ? '' : t.slice(sp + 1);
      const plugin = PLUGIN_BY_NAME[name];
      if (!plugin) {
        addLine({ kind: 'err', group: cur, text: `unknown command: /${name}` });
        return;
      }
      if (activePluginRef.current) {
        addLine({ kind: 'err', group: cur, text: `already running /${activePluginRef.current.name}` });
        return;
      }
      const handle = runPlugin(plugin, args, cur, {
        log: (text, kind = 'sys', g) => addLine({ kind, group: g ?? cur, text }),
        callDaemon: (cmd, extra) => call(cmd, extra),
      });
      activePluginRef.current = handle;
      setPluginName(plugin.name);
      addLine({ kind: 'sys', group: cur, text: `started /${plugin.name}` });
      handle.done.finally(() => {
        if (activePluginRef.current === handle) {
          activePluginRef.current = null;
          setPluginName(null);
          addLine({ kind: 'sys', group: cur, text: `/${plugin.name} finished` });
        }
      });
    } else if (t === '/config' || t.startsWith('/config ')) {
      // /config              → show current group's config
      // /config model=sonnet → set model for current group
      // /config model=       → clear model
      const args = t.length > 7 ? t.slice(8).trim() : '';
      const payload: Record<string, string> = { group: cur };
      if (args) {
        for (const tok of args.split(/\s+/)) {
          const eq = tok.indexOf('=');
          if (eq < 0) {
            addLine({ kind: 'err', group: cur, text: `bad config arg: ${tok} (use key=value)` });
            return;
          }
          payload[tok.slice(0, eq)] = tok.slice(eq + 1);
        }
      }
      try {
        const r = await call('config', payload);
        if (r.ok) {
          const cfg = r.config || {};
          const summary = Object.keys(cfg).length === 0
            ? '(default)'
            : Object.entries(cfg).map(([k,v]) => `${k}=${v}`).join('  ');
          addLine({ kind: 'sys', group: cur, text: `config[${cur}]: ${summary}` });
        } else {
          addLine({ kind: 'err', group: cur, text: `config: ${r.error}` });
        }
      } catch (e: any) {
        addLine({ kind: 'err', group: cur, text: `config: ${e.message || e}` });
      }
    } else {
      try {
        const r = await call('send', { group: cur, msg: t });
        if (!r.ok) addLine({ kind: 'err', group: cur, text: r.error });
        else refresh();  // pick up auto-spawned sidecar + create subscribe socket
      } catch (e: any) {
        addLine({ kind: 'err', group: cur, text: e.message || String(e) });
      }
    }
  };

  // chrome: header (1) + bordered input (3) + hint (1) = 5 rows
  const logRows = Math.max(1, size.rows - 5);
  const all = lines.filter(l => !l.group || l.group === cur);
  const streaming = streamBuf[cur];
  const reserveStream = streaming ? 1 : 0;
  const window = Math.max(1, logRows - reserveStream);

  // Build blocks (consecutive same-kind+same-group lines coalesced) and
  // pre-render the body of each so we can size them in rendered rows, not
  // logical lines. A markdown response of 4 source lines may render as 12
  // rows; the old line-based slice cut bloggily into block tops. Block-aware
  // slicing here keeps each visible block whole, except when one block alone
  // exceeds the window — in that case we keep its tail and drop its head.
  type RBlock = {
    kind: LogLine['kind']; group: string; ts?: number;
    rendered: string;     // already markdown-converted for response blocks
    rows: number;         // rendered-line count (separator not included)
    truncated?: boolean;
  };
  const sourceBlocks: { kind: LogLine['kind']; group: string; ts?: number; text: string }[] = [];
  for (const l of all) {
    const last = sourceBlocks[sourceBlocks.length - 1];
    if (last && last.kind === l.kind && last.group === l.group) {
      last.text += '\n' + l.text;
      if (last.ts === undefined && l.ts !== undefined) last.ts = l.ts;
    } else {
      sourceBlocks.push({ kind: l.kind, group: l.group, ts: l.ts, text: l.text });
    }
  }
  const allBlocks: RBlock[] = sourceBlocks.map(b => {
    const rendered = b.kind === 'response' ? md(b.text) : b.text;
    return { kind: b.kind, group: b.group, ts: b.ts, rendered, rows: rendered.split('\n').length };
  });

  // Walk from end accumulating rows + 1-row separators. Stop at window budget.
  const visibleBlocks: RBlock[] = [];
  let used = 0;
  for (let i = allBlocks.length - 1 - Math.min(scroll, Math.max(0, allBlocks.length - 1)); i >= 0; i--) {
    const b = allBlocks[i]!;
    const sep = visibleBlocks.length > 0 ? 1 : 0;
    if (used + b.rows + sep > window) {
      if (visibleBlocks.length === 0) {
        // Single block exceeds window → keep its tail.
        const ls = b.rendered.split('\n');
        const keep = Math.max(1, window);
        visibleBlocks.unshift({ ...b, rendered: ls.slice(-keep).join('\n'), rows: keep, truncated: true });
        used = keep;
      }
      break;
    }
    visibleBlocks.unshift(b);
    used += b.rows + sep;
  }

  const maxScroll = Math.max(0, allBlocks.length - 1);   // scroll in BLOCKS now
  const clampedScroll = Math.min(scroll, maxScroll);
  const showStream = clampedScroll === 0 && streaming;

  useInput((input, key) => {
    if (key.ctrl && input === 'c') {
      // Always exits TUI. To stop a running plugin without exiting, use
      // /stop-plugin <name>. Ctrl+C still aborts any active plugin via
      // process exit, so a runaway loop is never trapped.
      for (const s of subsRef.current.values()) s.end();
      activePluginRef.current?.abort.abort();
      exit();
      setTimeout(() => process.exit(0), 50);
      return;
    }
    if (key.tab) {
      const names = Object.keys(groups).sort();
      if (names.length > 1) {
        const i = names.indexOf(cur);
        const step = key.shift ? -1 : 1;
        const next = names[((i < 0 ? 0 : i) + step + names.length) % names.length]!;
        setCur(next);
      }
      return;
    }
    // Scroll units are now BLOCKS (one prompt or one response = one block),
    // since slicing is block-aware. PgUp/PgDn move 3 blocks; Shift+↑/↓ moves 1.
    if (key.pageUp)         setScroll(s => Math.min(maxScroll, s + 3));
    else if (key.pageDown)  setScroll(s => Math.max(0, s - 3));
    else if (key.shift && key.upArrow)   setScroll(s => Math.min(maxScroll, s + 1));
    else if (key.shift && key.downArrow) setScroll(s => Math.max(0, s - 1));
  });
  const spin = SPINNER[tick % SPINNER.length];
  const groupNames = Object.keys(groups).sort();

  return (
    <Box flexDirection="column" height={size.rows} width={size.cols}>
      {/* airline-style status line (powerline + nerd-font glyphs) */}
      <Box flexShrink={0} width={size.cols} justifyContent="space-between">
        {/* left: app · group · others */}
        <Box>
          <Text color="black" backgroundColor="cyan" bold>{'  clawson '}</Text>
          <Text color="cyan" backgroundColor="blue">{''}</Text>
          <Text color="white" backgroundColor="blue" bold>
            {'   '}{cur}{groups[cur]?.running ? ' ' : ' '}{' '}
          </Text>
          <Text color="blue" backgroundColor="black">{''}</Text>
          <Text color="gray" backgroundColor="black">
            {' '}{groupNames.filter(g => g !== cur).map(g =>
              `${g}${groups[g]?.running ? '' : ''}`
            ).join('  ') || '—'}{' '}
          </Text>
          <Text color="black">{''}</Text>
        </Box>
        {/* right: stream/idle · message count */}
        <Box>
          <Text color="black">{''}</Text>
          {(() => {
            // ctx is per-group (the active conversation's last input size).
            // budget is account-wide so it reads from globalMetric and shows
            // even when cur is stopped or has never had an API call yet.
            const cu  = metric?.usage ?? {};
            const ctx = (Number(cu.input_tokens) || 0)
                      + (Number(cu.cache_read_input_tokens) || 0)
                      + (Number(cu.cache_creation_input_tokens) || 0);
            const ctxFrac = CTX_WINDOW > 0 ? ctx / CTX_WINDOW : 0;
            const grl = globalMetric?.ratelimit ?? {};
            const f = (k: string): number | null => {
              const v = grl[k]; if (v == null) return null;
              const n = parseFloat(String(v)); return isNaN(n) ? null : n;
            };
            const u5h = f('anthropic-ratelimit-unified-5h-utilization');
            const u7d = f('anthropic-ratelimit-unified-7d-utilization');
            return (
              <>
                {ctx > 0 ? (
                  <Text color={pctColor(ctxFrac)} backgroundColor="black" bold>
                    {`  ctx ${(ctxFrac * 100).toFixed(0)}% `}
                  </Text>
                ) : null}
                {u5h != null ? (
                  <Text color={pctColor(u5h)} backgroundColor="black" bold>
                    {`  5h ${(u5h * 100).toFixed(0)}% `}
                  </Text>
                ) : null}
                {u7d != null ? (
                  <Text color={pctColor(u7d)} backgroundColor="black" bold>
                    {`  7d ${(u7d * 100).toFixed(0)}% `}
                  </Text>
                ) : null}
              </>
            );
          })()}
          {!connected ? (
            <Text color="red" backgroundColor="black" bold>{`  reconnecting ${spin} `}</Text>
          ) : null}
          {pluginName ? (
            <Text color="magenta" backgroundColor="black" bold>{`  ▶ /${pluginName} ${spin} `}</Text>
          ) : null}
          <Text color={streaming ? 'yellow' : 'gray'} backgroundColor="black">
            {streaming ? `   streaming ${spin} ` : '   idle '}
          </Text>
          <Text color="black" backgroundColor="cyan">{''}</Text>
          <Text color="black" backgroundColor="cyan" bold>
            {'   '}{lines.filter(l => l.group === cur).length}{'  '}
          </Text>
        </Box>
      </Box>

      {/* log — block-aware slicing: each visible block renders whole,
           or only its tail if a single block alone exceeds the window. */}
      <Box flexDirection="column" flexGrow={1} paddingX={1} overflow="hidden">
        {visibleBlocks.map((b, i) => {
          const stamp = b.ts ? fmtTime(b.ts) : '     ';
          const tsNode = <Text dimColor>{stamp} </Text>;
          const key = `${i}-${b.kind}-${b.ts ?? 0}`;
          const sep = i > 0 ? <Box key={`sep-${key}`} height={1} /> : null;
          const truncMark = b.truncated
            ? <Text dimColor>… (truncated){"\n"}</Text>
            : null;
          if (b.kind === 'prompt') {
            return (<React.Fragment key={key}>
              {sep}
              <Box>
                {tsNode}
                <Text color="cyan" bold>›  </Text>
                <Text color="cyan" wrap="wrap">{b.rendered}</Text>
              </Box>
            </React.Fragment>);
          }
          if (b.kind === 'err') {
            return (<React.Fragment key={key}>
              {sep}
              <Box>
                {tsNode}
                <Text color="red">▎  </Text>
                <Text color="red" wrap="wrap">{b.rendered}</Text>
              </Box>
            </React.Fragment>);
          }
          if (b.kind === 'sys') {
            return (<React.Fragment key={key}>
              {sep}
              <Box>{tsNode}<Text dimColor wrap="wrap">·  {b.rendered}</Text></Box>
            </React.Fragment>);
          }
          // response: markdown ANSI inside a left-bordered Box; the border
          // gives the unified left-bar look across all wrapped lines.
          return (<React.Fragment key={key}>
            {sep}
              <Box>
                {tsNode}
                <Box flexGrow={1} borderStyle="single" borderColor="gray"
                   borderTop={false} borderRight={false} borderBottom={false}
                   paddingLeft={1} flexDirection="column">
                {truncMark}
                <Text wrap="wrap">{b.rendered}</Text>
              </Box>
            </Box>
          </React.Fragment>);
        })}
        {showStream ? (
          <>
            {/* always pad before in-progress block if log doesn't already end on one */}
            {visibleBlocks.length > 0 && visibleBlocks[visibleBlocks.length - 1]!.kind !== 'response' ? (
              <Box height={1} />
            ) : null}
            <Box>
              <Text color="yellow">{spin}  </Text>
              <Text>{streaming}</Text>
            </Box>
          </>
        ) : null}
      </Box>

      {/* bordered input */}
      <Box flexShrink={0} borderStyle="round" borderColor="gray">
        <Box marginX={1}>
          <Text color="cyan" bold></Text>
        </Box>
        <TextInput value={input} onChange={setInput} onSubmit={onSubmit}
          placeholder="ask anything   (/new  /sw  /ls  /clear  /config  /burn <goal>  /stop-plugin <name>)" />
      </Box>

      {/* hint */}
      <Box flexShrink={0} paddingX={2}>
        <Text dimColor>{streaming ? ' streaming…' : ' enter to send'}</Text>
        <Text dimColor>   ·    pgup/pgdn scroll</Text>
        {clampedScroll > 0 ? (
          <Text color="yellow">   ·   ↑{clampedScroll}/{maxScroll}</Text>
        ) : null}
        <Text dimColor>   ·    ctrl+c to exit</Text>
      </Box>
    </Box>
  );
};

render(<App />);
