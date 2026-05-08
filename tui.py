#!/usr/bin/env python3
"""clawson TUI: thin client over the daemon's unix socket."""
import json, os, pathlib, socket, threading, time
from rich.markup import escape
from textual.app import App, ComposeResult
from textual.widgets import Input, RichLog, Static

HERE = pathlib.Path(__file__).parent.resolve()
ROOT = HERE / "groups"
SOCK_PATH = HERE / "clawson.sock"


def call(cmd, **kw):
    s = socket.socket(socket.AF_UNIX); s.connect(str(SOCK_PATH))
    f = s.makefile("rwb")
    f.write((json.dumps({"cmd": cmd, **kw}) + "\n").encode()); f.flush()
    resp = json.loads(f.readline())
    s.close()
    return resp


def tail(g, app):
    """Byte-level reader. Streams partial-line text via app._on_stream;
    on newline, completes the line via app._on_prompt or app._on_done."""
    p = ROOT / g / ".cs/log"
    p.parent.mkdir(parents=True, exist_ok=True); p.touch()
    f = open(p, "r", encoding="utf-8", errors="replace")
    f.seek(0, 2)
    inode = p.stat().st_ino
    line_buf = ""
    while True:
        try:
            cur_inode = p.stat().st_ino
            if cur_inode != inode:  # log truncated/replaced (sidecar restart)
                f.close()
                f = open(p, "r", encoding="utf-8", errors="replace")
                inode = cur_inode
                line_buf = ""
        except OSError:
            time.sleep(0.1); continue
        chunk = f.read()
        if not chunk:
            time.sleep(0.05); continue
        i = 0
        while i < len(chunk):
            j = chunk.find("\n", i)
            if j == -1:
                line_buf += chunk[i:]
                break
            line_buf += chunk[i:j]
            if line_buf.startswith(">>> "):
                app.call_from_thread(app._on_prompt, g, line_buf[4:])
            else:
                app.call_from_thread(app._on_done, g, line_buf)
            line_buf = ""
            i = j + 1
        # partial line: stream-update if it's a response (not a prompt)
        if line_buf and not line_buf.startswith(">"):
            app.call_from_thread(app._on_stream, g, line_buf)


class TUI(App):
    CSS = """
    Input{dock:bottom}
    #s{height:1;background:$accent}
    #stream{height:auto;color:$text;padding:0 1}
    RichLog{height:1fr}
    """

    def __init__(self):
        super().__init__()
        self.cur = "main"
        self.logw = RichLog(markup=True, wrap=True)
        self.streamw = Static("", id="stream")
        self.tails = set()
        self._sbuf = {}  # group -> currently-streaming text

    def compose(self) -> ComposeResult:
        yield Static("clawson", id="s")
        yield self.logw
        yield self.streamw
        yield Input(placeholder="msg | /new <g> | /sw <g> | /ls")

    def on_mount(self):
        self.query_one(Input).focus()
        self._refresh()

    def _refresh(self):
        try: groups = call("list").get("groups", {})
        except Exception as e:
            self.logw.write(f"[red]daemon unreachable:[/] {e}")
            self.query_one("#s", Static).update("[b]clawson[/]  [red]daemon down[/]")
            return
        for g in groups:
            if g not in self.tails: self._tail(g)
        self.query_one("#s", Static).update(
            f"[b]clawson[/]  cur=[cyan]{self.cur}[/]  groups={sorted(groups)}")

    def _tail(self, g):
        if g in self.tails: return
        self.tails.add(g)
        threading.Thread(target=tail, args=(g, self), daemon=True).start()

    def _bg(self, fn, *a):
        threading.Thread(target=fn, args=a, daemon=True).start()

    def _spawn(self, g):
        try: r = call("spawn", group=g)
        except Exception as e: r = {"ok": False, "error": repr(e)}
        if not r.get("ok"):
            self.call_from_thread(self.logw.write, f"[red]spawn {g} FAILED:[/] {r.get('error')}")
            return
        self._tail(g)
        self.call_from_thread(self.logw.write, f"[green]{g} ready[/]")
        self.call_from_thread(self._refresh)

    # -- stream handlers (called via app.call_from_thread from tail()) --
    def _on_prompt(self, g, msg):
        self._flush_stream(g)
        self.logw.write(f"\n[b cyan]>> {g}:[/] {escape(msg)}")

    def _on_stream(self, g, text):
        self._sbuf[g] = text
        if g == self.cur:
            self.streamw.update(escape(text))

    def _on_done(self, g, text):
        # newline arrived after streaming text
        self._sbuf.pop(g, None)
        if text:
            self.logw.write(escape(text))
        if g == self.cur:
            self.streamw.update("")

    def _flush_stream(self, g):
        text = self._sbuf.pop(g, None)
        if text:
            self.logw.write(escape(text))
        if g == self.cur:
            self.streamw.update("")

    def on_input_submitted(self, e: Input.Submitted):
        v = e.value.strip(); e.input.value = ""
        if not v: return
        if v.startswith("/new "):
            self._bg(self._spawn, v.split(None, 1)[1])
        elif v.startswith("/sw "):
            self.cur = v.split(None, 1)[1]
            self.streamw.update(escape(self._sbuf.get(self.cur, "")))
            self._refresh()
        elif v == "/ls":
            self._refresh()
        else:
            self._bg(lambda g=self.cur, m=v: call("send", group=g, msg=m))


if __name__ == "__main__":
    TUI().run()
