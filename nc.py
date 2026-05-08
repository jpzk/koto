#!/usr/bin/env python3
"""nanoclaw: TUI control plane for isolated claude-code containers."""
import atexit, base64, json, os, pathlib, subprocess, sys, threading
from textual.app import App, ComposeResult
from textual.widgets import Input, RichLog, Static

HERE = pathlib.Path(__file__).parent.resolve()
ROOT = HERE / "groups"
GROUPS_FILE = HERE / "groups.json"
IMAGE = "nanoclaw"
PORT_BASE = int(os.environ.get("PROXY_PORT", "8787"))
PROXY_HOST = os.environ.get("PROXY_HOST", "host.containers.internal")  # pasta default; "nc_host" inside DooD
NETWORK = os.environ.get("NC_NETWORK", "pasta")


def vol(g): return ROOT / g


def alloc_port(g):
    m = json.loads(GROUPS_FILE.read_text()) if GROUPS_FILE.exists() else {}
    if g not in m:
        m[g] = PORT_BASE + len(m)
        GROUPS_FILE.write_text(json.dumps(m))
    return m[g]


def ensure(g, main=False):
    v = vol(g); (v / ".nc").mkdir(parents=True, exist_ok=True)
    fifo = v / ".nc/in"
    if not fifo.exists(): os.mkfifo(fifo)
    port = alloc_port(g)
    name = f"nc_{g}"
    if subprocess.run(["podman","ps","-q","-f",f"name=^{name}$"],
                      capture_output=True, text=True).stdout.strip(): return
    args = ["podman","run","-d","--rm","--name",name,
            "--security-opt","label=disable",
            "--userns=keep-id",
            f"--network={NETWORK}",
            "-v",f"{v}:/workspace",
            "-e","ANTHROPIC_API_KEY=proxied",
            "-e","HOME=/workspace",
            "-e",f"ANTHROPIC_BASE_URL=http://{PROXY_HOST}:{port}"]
    if main: args += ["-v", f"{ROOT}:/peers"]
    args.append(IMAGE)
    subprocess.run(args, check=True, capture_output=True, text=True)


def send(g, msg):
    b = base64.b64encode(msg.encode()).decode()
    with open(vol(g) / ".nc/in", "w") as f: f.write(b + "\n")


def tail(g, app, log):
    p = vol(g) / ".nc/log"; p.parent.mkdir(parents=True, exist_ok=True); p.touch()
    proc = subprocess.Popen(["tail","-F","-n","0",str(p)],
                            stdout=subprocess.PIPE, text=True, bufsize=1)
    for line in proc.stdout:
        app.call_from_thread(log.write, f"[{g}] {line.rstrip()}")


class NC(App):
    CSS = "Input{dock:bottom}RichLog{height:1fr}#s{height:1;background:$accent}"

    def __init__(self):
        super().__init__()
        self.cur = "main"; self.logw = RichLog(markup=True); self.tails = set()

    def compose(self) -> ComposeResult:
        yield Static("nanoclaw", id="s"); yield self.logw
        yield Input(placeholder="msg | /new <g> | /sw <g> | /ls")

    def on_mount(self):
        alloc_port("main")  # ensure groups.json exists before proxy starts
        self._plog = open(HERE / "proxy.log", "ab", buffering=0)
        self._proxy = subprocess.Popen([sys.executable, str(HERE / "proxy.py")],
                                       stdout=self._plog, stderr=subprocess.STDOUT)
        atexit.register(self._proxy.terminate)
        self._status()
        self.logw.write("[dim]starting main...[/]")
        self._bg(self._spawn, "main", True)

    def _status(self):
        groups = sorted(p.name for p in ROOT.iterdir() if p.is_dir())
        self.query_one("#s", Static).update(
            f"[b]nanoclaw[/]  cur=[cyan]{self.cur}[/]  groups={groups}")

    def _tail(self, g):
        if g in self.tails: return
        self.tails.add(g)
        threading.Thread(target=tail, args=(g, self, self.logw), daemon=True).start()

    def _bg(self, fn, *a):
        threading.Thread(target=fn, args=a, daemon=True).start()

    def _spawn(self, g, main=False):
        ensure(g, main=main)
        self._tail(g)
        self.call_from_thread(self._status)

    def on_input_submitted(self, e: Input.Submitted):
        v = e.value.strip(); e.input.value = ""
        if not v: return
        if v.startswith("/new "):
            self._bg(self._spawn, v.split(None, 1)[1])
        elif v.startswith("/sw "):
            self.cur = v.split(None, 1)[1]; self._status()
        elif v == "/ls":
            self._status()
        else:
            self.logw.write(f"[dim]> {self.cur}: {v}[/]")
            self._bg(send, self.cur, v)


if __name__ == "__main__":
    NC().run()
