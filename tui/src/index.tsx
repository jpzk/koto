import React, { useEffect, useState, useRef } from 'react';
import { render, Box, Text, useStdout, useInput, useApp } from 'ink';
import TextInput from 'ink-text-input';
import * as net from 'node:net';

process.on('SIGINT',  () => process.exit(130));
process.on('SIGTERM', () => process.exit(143));

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

  useInput((input, key) => {
    if (key.ctrl && input === 'c') {
      for (const s of subsRef.current.values()) s.end();
      exit();
      setTimeout(() => process.exit(0), 50);
      return;
    }
    if (key.pageUp)         setScroll(s => Math.min(maxScroll, s + Math.floor(window / 2)));
    else if (key.pageDown)  setScroll(s => Math.max(0, s - Math.floor(window / 2)));
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
          <Text color={streaming ? 'yellow' : 'gray'} backgroundColor="black">
            {streaming ? `   streaming ${spin} ` : '   idle '}
          </Text>
          <Text color="black" backgroundColor="cyan">{''}</Text>
          <Text color="black" backgroundColor="cyan" bold>
            {'   '}{lines.filter(l => l.group === cur).length}{'  '}
          </Text>
        </Box>
      </Box>

      {/* log — turns rendered as blocks, separated by blank rows */}
      <Box flexDirection="column" flexGrow={1} paddingX={1} overflow="hidden">
        {visible.flatMap((l, i) => {
          const idx = all.length - visible.length + i;
          const prev = i > 0 ? visible[i - 1] : null;
          const out: React.ReactNode[] = [];
          // block boundary: insert a blank row before a turn change
          if (prev && (
            (l.kind === 'prompt' && prev.kind !== 'prompt') ||
            (l.kind === 'response' && prev.kind !== 'response') ||
            (l.kind !== prev.kind)
          )) {
            out.push(<Box key={`sep-${idx}`} height={1} />);
          }
          if (l.kind === 'prompt') {
            out.push(
              <Box key={idx}>
                <Text color="cyan" bold>›  </Text>
                <Text color="cyan">{l.text}</Text>
              </Box>
            );
          } else if (l.kind === 'err') {
            out.push(
              <Box key={idx}>
                <Text color="red">▎  </Text>
                <Text color="red">{l.text}</Text>
              </Box>
            );
          } else if (l.kind === 'sys') {
            out.push(<Text key={idx} dimColor>·  {l.text}</Text>);
          } else {
            // response — left bar makes consecutive lines read as one block
            out.push(
              <Box key={idx}>
                <Text dimColor>▎  </Text>
                <Text>{l.text}</Text>
              </Box>
            );
          }
          return out;
        })}
        {showStream ? (
          <>
            {/* always pad before in-progress block if log doesn't already end on one */}
            {visible.length > 0 && visible[visible.length - 1]!.kind !== 'response' ? (
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
          placeholder="ask anything   (/new <g>  /sw <g>  /ls)" />
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
