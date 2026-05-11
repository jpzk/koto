#!/usr/bin/env python3
"""clawson proxy: injects host credentials, logs metrics, hides API key from containers."""
import http.server, urllib.request, urllib.error, json, os, pathlib, time, subprocess, threading

HERE = pathlib.Path(__file__).parent
CRED = pathlib.Path(os.environ.get("CRED_PATH") or pathlib.Path.home() / ".claude/.credentials.json")
METRICS = HERE / "metrics.jsonl"
GROUPS_FILE = HERE / "groups.json"
GROUPS_DIR = HERE / "groups"
UPSTREAM = "https://api.anthropic.com"
KEY = os.environ.get("ANTHROPIC_API_KEY")
LOCK = threading.Lock()
LISTENERS = {}  # port -> server


def _refresh():
    try: subprocess.run(["claude", "-p", "ok"], capture_output=True, timeout=25)
    except Exception: pass


def auth_headers():
    if KEY:
        return {"x-api-key": KEY, "anthropic-version": "2023-06-01"}
    if not CRED.exists():
        raise RuntimeError("no credentials: set ANTHROPIC_API_KEY or run `claude /login`")
    with LOCK:
        d = json.loads(CRED.read_text())
        oa = d.get("claudeAiOauth", {})
        if oa.get("expiresAt", 0) / 1000 - time.time() < 60:
            _refresh()
            oa = json.loads(CRED.read_text()).get("claudeAiOauth", {})
    return {
        "authorization": f"Bearer {oa['accessToken']}",
        "anthropic-beta": "oauth-2025-04-20",
        "anthropic-version": "2023-06-01",
    }


def _log_append(group, data):
    """Append raw bytes to the group's sidecar log so the daemon's tailer
    picks them up. claude-code-cli filters `thinking` blocks out of its
    stream-json output, but the API itself sends them; we intercept here
    and route them into the same log file the sidecar writes to. The
    daemon understands `[[think_begin]]` / `[[think_end]] <words>` framing
    and routes lines between them as thinking events to subscribers.
    POSIX guarantees writes <= PIPE_BUF (~4KB) under O_APPEND are atomic,
    so this is race-safe with the sidecar's concurrent writes."""
    if not group: return
    p = GROUPS_DIR / group / ".cs" / "log"
    try:
        with open(p, "ab") as f: f.write(data)
    except Exception: pass


def log(group, path, status, hdrs, usage, dur):
    rec = {
        "ts": time.time(), "dur_ms": int(dur * 1000),
        "group": group, "path": path, "status": status,
        "request_id": hdrs.get("request-id") or hdrs.get("Request-Id"),
        "ratelimit": {k.lower(): v for k, v in hdrs.items()
                      if k.lower().startswith("anthropic-ratelimit")
                      or k.lower() == "anthropic-organization-id"},
        "usage": usage,
    }
    with open(METRICS, "a") as f: f.write(json.dumps(rec) + "\n")


class H(http.server.BaseHTTPRequestHandler):
    GROUP = "?"
    def log_message(self, *a): pass

    def _proxy(self, method):
        t0 = time.time()
        body = self.rfile.read(int(self.headers.get("content-length", 0))) if method in ("POST", "PUT") else None
        group = self.GROUP
        try: ah = auth_headers()
        except Exception as e:
            self.send_error(503, str(e)); return
        # build merged header dict: client headers, minus auth/hop-by-hop, plus our auth
        block = {"authorization","x-api-key","host","content-length","connection",
                 "transfer-encoding","accept-encoding"}
        h = {k: self.headers[k] for k in self.headers.keys() if k.lower() not in block}
        # merge anthropic-beta if both sides set it
        cb = next((v for k,v in h.items() if k.lower()=="anthropic-beta"), None)
        for k in list(h):
            if k.lower() == "anthropic-beta": del h[k]
        merged_beta = ",".join(filter(None, [cb, ah.pop("anthropic-beta", None)]))
        if merged_beta: h["anthropic-beta"] = merged_beta
        h.update(ah)
        h["accept-encoding"] = "identity"
        req = urllib.request.Request(UPSTREAM + self.path, data=body, method=method, headers=h)
        try: r = urllib.request.urlopen(req, timeout=600)
        except urllib.error.HTTPError as e: r = e
        rh = dict(r.headers)
        self.send_response(r.status)
        skip = {"content-encoding", "transfer-encoding", "content-length", "connection"}
        for k, v in rh.items():
            if k.lower() not in skip: self.send_header(k, v)
        self.end_headers()
        usage = {}
        in_thinking = False
        thinking_words = 0
        try:
            if "event-stream" in rh.get("Content-Type", ""):
                for line in r:
                    self.wfile.write(line); self.wfile.flush()
                    s = line.decode("utf-8", "ignore").strip()
                    if s.startswith("data:"):
                        try:
                            ev = json.loads(s[5:].strip())
                            for u in (ev.get("usage"), (ev.get("message") or {}).get("usage")):
                                if u: usage.update(u)
                            # Surface thinking blocks into the sidecar log so
                            # they show up in the TUI alongside the response.
                            t = ev.get("type", "")
                            if t == "content_block_start":
                                cb = ev.get("content_block", {})
                                if cb.get("type") == "thinking":
                                    in_thinking = True
                                    thinking_words = 0
                                    _log_append(group, b"[[think_begin]]\n")
                            elif t == "content_block_delta" and in_thinking:
                                d = ev.get("delta", {})
                                if d.get("type") == "thinking_delta":
                                    txt = d.get("thinking", "")
                                    if txt:
                                        _log_append(group, txt.encode("utf-8"))
                                        thinking_words += len(txt.split())
                            elif t == "content_block_stop" and in_thinking:
                                in_thinking = False
                                _log_append(group, f"\n[[think_end]] {thinking_words}\n".encode("utf-8"))
                        except Exception: pass
            else:
                data = r.read(); self.wfile.write(data)
                try:
                    if u := json.loads(data).get("usage"): usage.update(u)
                except Exception: pass
        except (BrokenPipeError, ConnectionResetError): pass
        log(group, self.path, r.status, rh, usage, time.time() - t0)

    def do_POST(self): self._proxy("POST")
    def do_GET(self): self._proxy("GET")


def _listen(bind, port, group):
    cls = type(f"H_{group}", (H,), {"GROUP": group})
    s = http.server.ThreadingHTTPServer((bind, port), cls)
    threading.Thread(target=s.serve_forever, daemon=True).start()
    LISTENERS[port] = s
    print(f"+ {group} -> {bind}:{port}", flush=True)


def _reload(bind):
    if not GROUPS_FILE.exists(): return
    try: m = json.loads(GROUPS_FILE.read_text())
    except Exception: return
    for g, port in m.items():
        if port not in LISTENERS: _listen(bind, port, g)


if __name__ == "__main__":
    bind = os.environ.get("BIND", "127.0.0.1")
    print(f"clawson-proxy bind={bind} -> {UPSTREAM}  metrics={METRICS}", flush=True)
    last = 0
    while True:
        try:
            mt = GROUPS_FILE.stat().st_mtime
            if mt != last: last = mt; _reload(bind)
        except FileNotFoundError: pass
        time.sleep(1)
