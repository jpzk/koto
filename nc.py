#!/usr/bin/env python3
"""clawson daemon: orchestrates sidecars + proxy, exposes unix socket API."""
import atexit, base64, json, os, pathlib, signal, socket, subprocess, sys, threading, time

HERE = pathlib.Path(__file__).parent.resolve()
ROOT = HERE / "groups"
GROUPS_FILE = HERE / "groups.json"
SOCK_PATH = HERE / "clawson.sock"
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
            "-e","ANTHROPIC_API_KEY=proxied",
            "-e","HOME=/workspace",
            "-e",f"ANTHROPIC_BASE_URL=http://{PROXY_HOST}:{port}"]
    if main: args += ["-v", f"{ROOT}:/peers"]
    args.append(IMAGE)
    subprocess.run(args, check=True, capture_output=True, text=True)
    return port


def send(g, msg):
    b = base64.b64encode(msg.encode()).decode()
    with open(vol(g) / ".cs/in", "w") as f: f.write(b + "\n")


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
    line = (json.dumps({"event": event, "group": g, **kw}) + "\n").encode()
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


def _tail_log(g):
    p = vol(g) / ".cs/log"
    p.parent.mkdir(parents=True, exist_ok=True); p.touch()
    f = open(p, "r", encoding="utf-8", errors="replace"); f.seek(0, 2)
    inode = p.stat().st_ino
    buf = ""
    while True:
        try:
            cur = p.stat().st_ino
            if cur != inode:
                f.close(); f = open(p, "r", encoding="utf-8", errors="replace")
                inode = cur; buf = ""
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
            if buf.startswith(">>> "): _emit(g, "prompt", msg=buf[4:])
            else:                       _emit(g, "done", text=buf)
            buf = ""; i = j + 1
        if buf and not buf.startswith(">"):
            _emit(g, "stream", text=buf)


def _ensure_tail(g):
    with SUBS_LOCK:
        if g in TAILS: return
        TAILS.add(g)
    threading.Thread(target=_tail_log, args=(g,), daemon=True).start()


def _spawn(req): return {"ok": True, "port": ensure(req["group"], req.get("main", False))}
def _send(req):  send(req["group"], req["msg"]); return {"ok": True}
def _list(req):  return {"ok": True, "groups": list_groups()}
def _stop(req):  stop(req["group"]); return {"ok": True}

HANDLERS = {"spawn": _spawn, "send": _send, "list": _list, "stop": _stop}


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
