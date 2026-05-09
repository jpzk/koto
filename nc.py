#!/usr/bin/env python3
"""clawson daemon: orchestrates sidecars + proxy, exposes unix socket API."""
import atexit, base64, datetime, json, os, pathlib, signal, socket, subprocess, sys, threading, time

HERE = pathlib.Path(__file__).parent.resolve()
ROOT = HERE / "groups"
GROUPS_FILE = HERE / "groups.json"
SOCK_DIR  = HERE / "run"
SOCK_PATH = SOCK_DIR / "clawson.sock"
IMAGE = "clawson"
PORT_BASE = int(os.environ.get("PROXY_PORT", "8787"))
PROXY_HOST = os.environ.get("PROXY_HOST", "host.containers.internal")
NETWORK = os.environ.get("NC_NETWORK", "pasta")


def vol(g): return ROOT / g


def alloc_port(g):
    m = json.loads(GROUPS_FILE.read_text()) if GROUPS_FILE.exists() else {}
    if g not in m:
        m[g] = PORT_BASE + len(m)
        GROUPS_FILE.write_text(json.dumps(m))
    return m[g]


def ensure(g, main=False):
    v = vol(g); (v / ".cs").mkdir(parents=True, exist_ok=True)
    fifo = v / ".cs/in"
    if not fifo.exists(): os.mkfifo(fifo)
    port = alloc_port(g)
    name = f"cs_{g}"
    if subprocess.run(["podman","ps","-q","-f",f"name=^{name}$"],
                      capture_output=True, text=True).stdout.strip():
        return port
    args = ["podman","run","-d","--rm","--name",name,
            "--security-opt","label=disable",
            "--userns=keep-id",
            f"--network={NETWORK}",
            "-v",f"{v}:/workspace",
            "-v",f"{HERE}/entrypoint.sh:/e.sh:ro",
            "-v",f"{HERE}/stream_filter.js:/stream_filter.js:ro",
            "-v","/etc/localtime:/etc/localtime:ro",
            "-e","ANTHROPIC_API_KEY=proxied",
            "-e","HOME=/workspace",
            "-e","SHELL=/bin/bash",
            "-e",f"ANTHROPIC_BASE_URL=http://{PROXY_HOST}:{port}"]
    if (HERE / "prompts/global.md").exists():
        args += ["-v", f"{HERE}/prompts/global.md:/prompts/global.md:ro"]
    if main: args += ["-v", f"{ROOT}:/peers"]
    args.append(IMAGE)
    subprocess.run(args, check=True, capture_output=True, text=True)
    return port


METRICS_FILE = HERE / "metrics.jsonl"


def _latest_metric(g):
    """Tail metrics.jsonl, return last entry whose group matches g (or None)."""
    if not METRICS_FILE.exists(): return None
    try:
        size = METRICS_FILE.stat().st_size
        with open(METRICS_FILE, "rb") as f:
            f.seek(max(0, size - 65536))
            tail = f.read().decode("utf-8", errors="replace")
        for line in reversed(tail.splitlines()):
            try:
                m = json.loads(line)
                if m.get("group") == g: return m
            except Exception: continue
    except Exception: return None
    return None


def _fmt_dur(s):
    s = int(s)
    if s < 0:    return "now"
    if s < 60:   return f"{s}s"
    if s < 3600: return f"{s//60}m"
    if s < 86400:return f"{s//3600}h{(s%3600)//60}m"
    return f"{s//86400}d{(s%86400)//3600}h"


def _context_block(m):
    rl = m.get("ratelimit", {}) or {}
    u  = m.get("usage", {}) or {}
    now = time.time()
    def util(k):
        try: return f"{float(rl[k])*100:.0f}%"
        except Exception: return "?"
    def resets(k):
        try: return _fmt_dur(int(rl[k]) - now)
        except Exception: return "?"
    nowdt = datetime.datetime.now().astimezone()
    return (
        "<clawson-context>\n"
        f"now: {nowdt.isoformat(timespec='seconds')} ({nowdt.strftime('%A')})\n"
        f"rate-limit: 5h={util('anthropic-ratelimit-unified-5h-utilization')} "
        f"(resets {resets('anthropic-ratelimit-unified-5h-reset')}) | "
        f"7d={util('anthropic-ratelimit-unified-7d-utilization')} "
        f"(resets {resets('anthropic-ratelimit-unified-7d-reset')})\n"
        f"overage: {rl.get('anthropic-ratelimit-unified-overage-status', '?')}\n"
        f"last-call: in={u.get('input_tokens',0)} "
        f"cache_rd={u.get('cache_read_input_tokens',0)} "
        f"cache_cr={u.get('cache_creation_input_tokens',0)} "
        f"out={u.get('output_tokens',0)} dur={m.get('dur_ms','?')}ms\n"
        "</clawson-context>"
    )


def send(g, msg):
    # Idempotently ensure the sidecar is up — auto-spawns if user did /sw to
    # a stopped or never-spawned group and is now sending a message.
    ensure(g, main=(g == "main"))
    fifo = vol(g) / ".cs/in"
    log  = vol(g) / ".cs/log"

    # Daemon writes the `>>> ` marker with the *original* message so the TUI
    # log stays clean; the augmented version (with rate-limit context) goes
    # only to the FIFO → claude.
    # The `[ts:<epoch-ms>]` line preceding it lets the tail/history parser
    # attach an accurate timestamp on history replay.
    try:
        with open(log, "a") as f:
            f.write(f"[ts:{int(time.time()*1000)}]\n>>> {msg}\n")
    except Exception: pass

    metric = _latest_metric(g)
    augmented = (_context_block(metric) + "\n\n" + msg) if metric else msg
    # The sidecar starts asynchronously; retry the FIFO open until the
    # entrypoint attaches as a reader, or give up after 5s.
    deadline = time.monotonic() + 5.0
    while True:
        try:
            fd = os.open(fifo, os.O_WRONLY | os.O_NONBLOCK); break
        except OSError as e:
            if e.errno != 6:  # not ENXIO
                raise
            if time.monotonic() > deadline:
                raise RuntimeError(f"group '{g}' sidecar didn't attach FIFO within 5s")
            time.sleep(0.1)
    try:
        os.set_blocking(fd, True)
        b = base64.b64encode(augmented.encode()).decode()
        os.write(fd, (b + "\n").encode())
    finally:
        os.close(fd)


def list_groups():
    out = {}
    if GROUPS_FILE.exists():
        for g, port in json.loads(GROUPS_FILE.read_text()).items():
            r = subprocess.run(["podman","ps","-q","-f",f"name=^cs_{g}$"],
                               capture_output=True, text=True)
            out[g] = {"port": port, "running": bool(r.stdout.strip())}
    return out


def stop(g):
    subprocess.run(["podman","rm","-f",f"cs_{g}"], capture_output=True)


# ---- streaming: tail group log files, fan out to subscribers --------------
SUBS_LOCK = threading.Lock()
SUBS = {}        # group -> set of file objects (writers) listening for events
TAILS = set()    # groups whose tail thread is already running


def _emit(g, event, **kw):
    ts = kw.pop("ts_override", None)
    if ts is None: ts = time.time()
    line = (json.dumps({"event": event, "group": g, "ts": ts, **kw}) + "\n").encode()
    with SUBS_LOCK: subs = list(SUBS.get(g, ()))
    dead = []
    for f in subs:
        try: f.write(line); f.flush()
        except Exception: dead.append(f)
    if dead:
        with SUBS_LOCK:
            s = SUBS.get(g)
            if s:
                for f in dead: s.discard(f)
        for f in dead:
            try: f.close()
            except Exception: pass


_TS_RE = __import__("re").compile(r"^\[ts:(\d+)\]$")


def _parse_ts_line(line):
    m = _TS_RE.match(line)
    if not m: return None
    try: return int(m.group(1)) / 1000.0
    except Exception: return None


def _tail_log(g):
    p = vol(g) / ".cs/log"
    p.parent.mkdir(parents=True, exist_ok=True); p.touch()
    f = open(p, "r", encoding="utf-8", errors="replace"); f.seek(0, 2)
    inode = p.stat().st_ino
    buf = ""
    pending_ts = None    # ts marker captured, applies to the next non-marker line
    while True:
        try:
            cur = p.stat().st_ino
            if cur != inode:
                f.close(); f = open(p, "r", encoding="utf-8", errors="replace")
                inode = cur; buf = ""; pending_ts = None
        except OSError:
            time.sleep(0.1); continue
        chunk = f.read()
        if not chunk: time.sleep(0.05); continue
        i = 0
        while i < len(chunk):
            j = chunk.find("\n", i)
            if j == -1:
                buf += chunk[i:]; break
            buf += chunk[i:j]
            ts_marker = _parse_ts_line(buf)
            if ts_marker is not None:
                pending_ts = ts_marker
            elif buf.startswith(">>> "):
                _emit(g, "prompt", msg=buf[4:], ts_override=pending_ts)
                pending_ts = None
            else:
                _emit(g, "done", text=buf, ts_override=pending_ts)
                pending_ts = None
            buf = ""; i = j + 1
        if buf and not buf.startswith(">") and not buf.startswith("[ts:"):
            _emit(g, "stream", text=buf)


def _ensure_tail(g):
    with SUBS_LOCK:
        if g in TAILS: return
        TAILS.add(g)
    threading.Thread(target=_tail_log, args=(g,), daemon=True).start()


def _read_history(g):
    """Parse .cs/log into the same event shape the live tail emits.

    Lines preceded by `[ts:<ms>]` get the precise stamp. Lines from before
    the [ts:] markers were introduced fall back to the file's mtime — same
    for all of them, but at least roughly correct ("last activity time")
    rather than rendering as a blank gutter."""
    p = vol(g) / ".cs/log"
    if not p.exists(): return []
    try:
        fallback_ts = p.stat().st_mtime
        text = p.read_text(encoding="utf-8", errors="replace")
    except Exception:
        return []
    events = []
    pending_ts = None
    for line in text.split("\n"):
        if not line: continue
        ts_marker = _parse_ts_line(line)
        if ts_marker is not None:
            pending_ts = ts_marker; continue
        ev = {"group": g, "historical": True,
              "ts": pending_ts if pending_ts is not None else fallback_ts}
        if line.startswith(">>> "):
            ev["event"] = "prompt"; ev["msg"] = line[4:]
        else:
            ev["event"] = "done"; ev["text"] = line
        events.append(ev)
        pending_ts = None
    return events


CONFIG_KEYS = ("model", "effort")  # whitelist what `config` accepts


def _config(req):
    """Get or merge per-group config. Empty string clears a field.
    {"cmd":"config","group":"main"}                     → returns current
    {"cmd":"config","group":"main","model":"sonnet"}    → sets, returns merged
    {"cmd":"config","group":"main","model":""}          → clears `model`
    """
    g = req["group"]
    p = vol(g) / ".cs/config.json"
    p.parent.mkdir(parents=True, exist_ok=True)
    cfg = {}
    if p.exists():
        try: cfg = json.loads(p.read_text())
        except Exception: cfg = {}
    changed = False
    for k in CONFIG_KEYS:
        if k in req:
            v = req[k]
            if v is None or v == "":
                if cfg.pop(k, None) is not None: changed = True
            else:
                if cfg.get(k) != v: changed = True
                cfg[k] = v
    if changed: p.write_text(json.dumps(cfg))
    return {"ok": True, "config": cfg}


def _spawn(req): return {"ok": True, "port": ensure(req["group"], req.get("main", False))}
def _send(req):  send(req["group"], req["msg"]); return {"ok": True}
def _list(req):  return {"ok": True, "groups": list_groups()}
def _stop(req):  stop(req["group"]); return {"ok": True}
def _hist(req):  return {"ok": True, "events": _read_history(req["group"])}

HANDLERS = {"spawn": _spawn, "send": _send, "list": _list, "stop": _stop,
            "history": _hist, "config": _config}


def serve(client):
    f = client.makefile("rwb")
    subscribed = False
    try:
        for line in f:
            try:
                req = json.loads(line)
                cmd = req["cmd"]
                if cmd == "subscribe":
                    g = req["group"]
                    _ensure_tail(g)
                    with SUBS_LOCK: SUBS.setdefault(g, set()).add(f)
                    f.write((json.dumps({"ok": True, "subscribed": g}) + "\n").encode()); f.flush()
                    subscribed = True
                    return
                resp = HANDLERS[cmd](req)
            except KeyError as e:
                resp = {"ok": False, "error": f"bad cmd: {e}"}
            except Exception as e:
                resp = {"ok": False, "error": f"{type(e).__name__}: {e}"}
            f.write((json.dumps(resp) + "\n").encode()); f.flush()
    finally:
        if not subscribed:
            try: client.close()
            except Exception: pass


def main():
    ROOT.mkdir(parents=True, exist_ok=True)
    SOCK_DIR.mkdir(parents=True, exist_ok=True)
    alloc_port("main")
    plog = open(HERE / "proxy.log", "ab", buffering=0)
    proxy = subprocess.Popen([sys.executable, str(HERE / "proxy.py")],
                             stdout=plog, stderr=subprocess.STDOUT)
    atexit.register(proxy.terminate)
    ensure("main", main=True)

    if SOCK_PATH.exists(): SOCK_PATH.unlink()
    s = socket.socket(socket.AF_UNIX); s.bind(str(SOCK_PATH)); s.listen(8)
    os.chmod(SOCK_PATH, 0o660)
    print(f"clawsond ready  socket={SOCK_PATH}", flush=True)

    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    while True:
        try: c, _ = s.accept()
        except (KeyboardInterrupt, OSError): break
        threading.Thread(target=serve, args=(c,), daemon=True).start()


if __name__ == "__main__":
    main()
