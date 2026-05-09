import React, { useEffect, useState, useRef } from 'react';
import { render, Box, Text, useStdout, useInput } from 'ink';
import TextInput from 'ink-text-input';
import * as net from 'node:net';

const SOCK = process.env.SOCK_PATH || '/sock';
const MAX_LINES = 500;
const SPINNER = ['⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'];

type Groups = Record<string, { port: number; running: boolean }>;
type LogLine = { kind: 'prompt' | 'response' | 'sys' | 'err'; group: string; text: string };
type Event = { event: 'prompt' | 'stream' | 'done'; group: string; msg?: string; text?: string };

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

function subscribe(group: string, onEvent: (e: Event) => void, onErr: (msg: string) => void): net.Socket {
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
  return c;
}

const App = () => {
  const { stdout } = useStdout();
  const [size, setSize] = useState({ rows: stdout.rows, cols: stdout.columns });
  const [groups, setGroups] = useState<Groups>({});
  const [cur, setCur] = useState('main');
  const [lines, setLines] = useState<LogLine[]>([]);
  const [streamBuf, setStreamBuf] = useState<Record<string, string>>({});
  const [input, setInput] = useState('');
  const [tick, setTick] = useState(0);
  const [scroll, setScroll] = useState(0); // lines above bottom; 0 = pinned to bottom
  const subsRef = useRef<Map<string, net.Socket>>(new Map());

  useEffect(() => {
    const onResize = () => setSize({ rows: stdout.rows, cols: stdout.columns });
    stdout.on('resize', onResize);
    return () => { stdout.off('resize', onResize); };
  }, [stdout]);

  useEffect(() => {
    const id = setInterval(() => setTick(t => t + 1), 80);
    return () => clearInterval(id);
  }, []);

  const addLine = (l: LogLine) => setLines(ls => {
    const next = ls.concat(l);
    return next.length > MAX_LINES ? next.slice(-MAX_LINES) : next;
  });

  const onEvent = (ev: Event) => {
    if (ev.event === 'prompt') {
      setStreamBuf(b => {
        const cur = b[ev.group];
        if (cur) addLine({ kind: 'response', group: ev.group, text: cur });
        const { [ev.group]: _, ...rest } = b;
        return rest;
      });
      addLine({ kind: 'prompt', group: ev.group, text: ev.msg || '' });
    } else if (ev.event === 'stream') {
      setStreamBuf(b => ({ ...b, [ev.group]: ev.text || '' }));
    } else if (ev.event === 'done') {
      setStreamBuf(b => { const { [ev.group]: _, ...rest } = b; return rest; });
      if (ev.text) addLine({ kind: 'response', group: ev.group, text: ev.text });
    }
  };

  const refresh = async () => {
    try {
      const r = await call('list');
      const gs: Groups = r.groups || {};
      setGroups(gs);
      for (const g of Object.keys(gs)) {
        if (!subsRef.current.has(g)) {
          subsRef.current.set(g, subscribe(g, onEvent,
            msg => addLine({ kind: 'err', group: g, text: msg })));
        }
      }
    } catch (e: any) {
      addLine({ kind: 'err', group: '', text: `daemon: ${e.message || e}` });
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
      setCur(t.slice(4).trim());
    } else if (t === '/ls') {
      refresh();
    } else {
      try {
        const r = await call('send', { group: cur, msg: t });
        if (!r.ok) addLine({ kind: 'err', group: cur, text: r.error });
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
  const maxScroll = Math.max(0, all.length - window);
  const clampedScroll = Math.min(scroll, maxScroll);
  const end = all.length - clampedScroll;
  const visible = all.slice(Math.max(0, end - window), end);
  const showStream = clampedScroll === 0 && streaming;

  useInput((_, key) => {
    if (key.pageUp)         setScroll(s => Math.min(maxScroll, s + Math.floor(window / 2)));
    else if (key.pageDown)  setScroll(s => Math.max(0, s - Math.floor(window / 2)));
    else if (key.shift && key.upArrow)   setScroll(s => Math.min(maxScroll, s + 1));
    else if (key.shift && key.downArrow) setScroll(s => Math.max(0, s - 1));
  });
  const spin = SPINNER[tick % SPINNER.length];
  const groupNames = Object.keys(groups).sort();

  return (
    <Box flexDirection="column" height={size.rows} width={size.cols}>
      {/* header */}
      <Box flexShrink={0} paddingX={1}>
        <Text dimColor>clawson</Text>
        <Text dimColor>  ·  </Text>
        <Text color="cyan">{cur}</Text>
        <Text dimColor>  ·  </Text>
        <Text dimColor>{groupNames.map(g => g === cur ? `[${g}]` : g).join(' ')}</Text>
      </Box>

      {/* log */}
      <Box flexDirection="column" flexGrow={1} paddingX={1} overflow="hidden">
        {visible.map((l, i) => {
          const idx = all.length - visible.length + i;
          if (l.kind === 'prompt') {
            return (
              <Box key={idx}>
                <Text color="cyan" bold>›  </Text>
                <Text>{l.text}</Text>
              </Box>
            );
          }
          if (l.kind === 'err') {
            return <Text key={idx} color="red">  {l.text}</Text>;
          }
          if (l.kind === 'sys') {
            return <Text key={idx} dimColor>  {l.text}</Text>;
          }
          return <Text key={idx}>  {l.text}</Text>;
        })}
        {showStream ? (
          <Box>
            <Text color="yellow">{spin}  </Text>
            <Text>{streaming}</Text>
          </Box>
        ) : null}
      </Box>

      {/* bordered input */}
      <Box flexShrink={0} borderStyle="round" borderColor="gray">
        <Box marginX={1}>
          <Text color="cyan" bold>›</Text>
        </Box>
        <TextInput value={input} onChange={setInput} onSubmit={onSubmit}
          placeholder="ask anything   (/new <g>  /sw <g>  /ls)" />
      </Box>

      {/* hint */}
      <Box flexShrink={0} paddingX={2}>
        <Text dimColor>{streaming ? 'streaming…' : 'enter to send'}</Text>
        <Text dimColor>   ·   pgup/pgdn scroll</Text>
        {clampedScroll > 0 ? (
          <Text color="yellow">   ·   ↑{clampedScroll}/{maxScroll}</Text>
        ) : null}
        <Text dimColor>   ·   ctrl+c to exit</Text>
      </Box>
    </Box>
  );
};

render(<App />);
