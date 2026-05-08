import React, { useEffect, useState, useRef } from 'react';
import { render, Box, Text } from 'ink';
import TextInput from 'ink-text-input';
import * as net from 'node:net';

const SOCK = process.env.SOCK_PATH || '/sock';
const MAX_LINES = 200;
const VISIBLE = 40;

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
  const [groups, setGroups] = useState<Groups>({});
  const [cur, setCur] = useState('main');
  const [lines, setLines] = useState<LogLine[]>([]);
  const [streamBuf, setStreamBuf] = useState<Record<string, string>>({});
  const [input, setInput] = useState('');
  const subsRef = useRef<Map<string, net.Socket>>(new Map());

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
        if (r.ok) addLine({ kind: 'sys', group: '', text: `${g} ready` });
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

  const visible = lines.filter(l => !l.group || l.group === cur).slice(-VISIBLE);
  const streaming = streamBuf[cur];

  return (
    <Box flexDirection="column">
      <Box>
        <Text backgroundColor="blue" bold> clawson </Text>
        <Text>  cur=</Text><Text color="cyan">{cur}</Text>
        <Text>  groups=[{Object.keys(groups).sort().join(', ')}]</Text>
      </Box>
      <Box flexDirection="column" marginTop={1}>
        {visible.map((l, i) => {
          const color = l.kind === 'prompt' ? 'cyan'
            : l.kind === 'err' ? 'red'
            : l.kind === 'sys' ? 'green' : undefined;
          return (
            <Text key={i} color={color} bold={l.kind === 'prompt'}>
              {l.kind === 'prompt' ? `>> ${l.group}: ` : ''}{l.text}
            </Text>
          );
        })}
        {streaming ? <Text dimColor>{streaming}</Text> : null}
      </Box>
      <Box marginTop={1}>
        <Text>› </Text>
        <TextInput value={input} onChange={setInput} onSubmit={onSubmit}
          placeholder="msg | /new <g> | /sw <g> | /ls" />
      </Box>
    </Box>
  );
};

render(<App />);
