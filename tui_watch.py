#!/usr/bin/env python3
"""Run tui.py as a subprocess; restart it whenever tui.py changes on disk.
Used by `make tui` so editing the TUI re-launches automatically. Stdlib only."""
import pathlib, signal, subprocess, sys, time

HERE = pathlib.Path(__file__).parent.resolve()
TARGET = HERE / "tui.py"


def mt(): return TARGET.stat().st_mtime if TARGET.exists() else 0


def spawn():
    return subprocess.Popen([sys.executable, str(TARGET)])


def stop(p):
    if p.poll() is not None: return
    p.send_signal(signal.SIGTERM)
    try: p.wait(timeout=3)
    except subprocess.TimeoutExpired: p.kill(); p.wait()


proc = spawn()
last = mt()
try:
    while True:
        time.sleep(0.5)
        if proc.poll() is not None:
            sys.exit(proc.returncode or 0)
        cur = mt()
        if cur != last:
            last = cur
            stop(proc)
            proc = spawn()
            last = mt()
except KeyboardInterrupt:
    stop(proc)
