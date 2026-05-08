#!/usr/bin/env python3
"""clawson TUI: thin client over the daemon's unix socket."""
import json, os, pathlib, socket, subprocess, threading
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


def tail(g, app, log):
    p = ROOT / g / ".cs/log"
    p.parent.mkdir(parents=True, exist_ok=True); p.touch()
    proc = subprocess.Popen(["tail","-F","-n","0",str(p)],
                            stdout=subprocess.PIPE, text=True, bufsize=1)
    for line in proc.stdout:
        app.call_from_thread(log.write, f"[{g}] {line.rstrip()}")


class TUI(App):
    CSS = "Input{dock:bottom}RichLog{height:1fr}#s{height:1;background:$accent}"

    def __init__(self):
        super().__init__()
        self.cur = "main"; self.logw = RichLog(markup=True); self.tails = set()

    def compose(self) -> ComposeResult:
        yield Static("clawson", id="s"); yield self.logw
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
        threading.Thread(target=tail, args=(g, self, self.logw), daemon=True).start()

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

    def on_input_submitted(self, e: Input.Submitted):
        v = e.value.strip(); e.input.value = ""
        if not v: return
        if v.startswith("/new "):
            self._bg(self._spawn, v.split(None, 1)[1])
        elif v.startswith("/sw "):
            self.cur = v.split(None, 1)[1]; self._refresh()
        elif v == "/ls":
            self._refresh()
        else:
            self.logw.write(f"[dim]> {self.cur}: {v}[/]")
            self._bg(lambda g=self.cur, m=v: call("send", group=g, msg=m))


if __name__ == "__main__":
    TUI().run()
