#!/usr/bin/env python3
"""tuiwalk — release gate for koto-tui's frame integrity.

Drives the real koto-tui image in a pty, feeds its output to a VT emulator
(pyte), and pages through transcripts at several terminal sizes counting
the two things a terminal does when a frame is wrong:

  WRAP    a row was emitted wider than the terminal (the terminal
          auto-wrapped it — everything below shifts down a row)
  SCROLL  the frame was taller than the terminal (the terminal scrolled —
          the whole screen shifts up a row)

Either one is a failing exit. Every Go-side width check agreed a frame was
exact while the terminal still wrapped it (a raw tab, 2026-08-29, 5f68d03):
the disagreement is with the terminal, so it has to be a terminal that
judges. pyte is a faithful VT emulator with a real wcwidth.

By default the walk spawns a throwaway group, injects a fixture transcript
of the known hard cases straight into its host-side log (no LLM call), walks
it, and destroys it. --all additionally walks every existing group's real
history, READ-ONLY: it only ever submits a `/sw <group>` it has verified on
the input row, never presses Enter otherwise, and never sends ESC in chat
mode (which would interrupt a running turn).

Non-destructive by construction — the guarantees, in one place:
  * the only mutating RPCs are Spawn and Destroy of the fixture group, whose
    name is the constant FIXTURE_GROUP; nothing else is ever created,
    destroyed, stopped, cleared or configured
  * a fixture group that already exists is reused, never re-created, and is
    refused if it carries a prompt.md (i.e. someone made it a real group)
  * the keys sent are: the characters of `/sw <group>` and `/exit`, Enter,
    Tab, PgUp, Home/End, Delete/Backspace, and the answer to the terminal's
    cursor-position query — no free text, no ctrl chords
  * Enter is pressed only after the input row on screen provably shows
    exactly the command typed (input_row_is), so a recalled prompt or a
    stray draft can never be submitted
  * ESC is never sent: in chat mode it interrupts the group's running turn
  * --all reads other groups' history through the TUI; it writes nothing

Needs: the daemon up (make host-run), the koto-tui image built (make
tui-build), podman, python3 with pyte (make tui-walk sets up the venv).
"""
import argparse, fcntl, os, pty, re, select, signal, struct, subprocess, sys, tempfile, termios, time

try:
    import pyte
except ImportError:
    sys.exit("tuiwalk: python module 'pyte' missing — run via `make tui-walk`")

HERE = os.path.abspath(os.path.join(os.path.dirname(__file__), "..", ".."))
FIXTURE_GROUP = "walk-fixture"
DEFAULT_SIZES = "40x140,24x80,56x118,66x146"

ESC = "\x1b"


def esc(n):
    return ESC + "[" + n + "m"


# The fixture: every class of content that has broken a frame or plausibly
# could. Each entry is one response body (markdown source as the agent
# would emit it). Keep adding to this when a new glitch is found.
FIXTURE_BODIES = [
    "7d\t2026-08-27\tr=0.881\tz=1.84\tdelta=-0.004\tdelta_sd=-0.01\n30d\t2026-08-27\tr=0.524\tz=1.89",
    "```\n" + "\n".join("%d\tcol\tvalue\t%s" % (i, "x" * (i * 7 % 40)) for i in range(20)) + "\n```",
    "# Heading one\n\nA paragraph that ends in the letter m\n\nAnother that ends in from",
    "| col | value |\n|---|---|\n| a | 1 |\n| very long cell text " + "x" * 200 + " | 2 |",
    "```ansi\n" + esc("38;2;255;0;0") + "red" + esc("48;2;0;0;255") + " on blue " + esc("7") + "reverse" + esc("0") + " plain\n" + esc("1;4") + "bold ul" + esc("0") + "\n```",
    "```ansi\nbroken \x1b[38;2;12 tail\x1b[ \x1b\x1b[31m smuggled? \x1b]0;title\x07 osc \x1bP dcs \x1b\\ done\n```",
    "raw escapes in prose: \x1b[31mred\x1b[0m and \x1b[1mbold\x1b[0m mid-sentence",
    "* item one\n* item two with `inline code` and **bold**\n  * nested\n\n> quote line",
    "Long unbroken " + "abcdefghij" * 60,
    "CJK 中文测试 日本語 한국어 😀🎉 emoji 🧠📤🔔⏳✅ text-presentation 🌡🗂🛡👁⚠✓⚙☀ kaomoji (˶˃ ᵕ ˂˶) ¯\\_(ツ)_/¯",
    "combining: é ñ ä zero-width: a​b‌c‍d bidi: ‮abc‬",
    "1. one\n2. two\n3. three\n\n---\n\n## H2 ends in m\n### H3\n#### H4\n##### H5",
    "text with literal escape-looking bits: [38;2;1;2;3m and \\x1b[0m and ESC[K",
    "\t tabs\tin\ttext\t\tand trailing spaces    \n\n\n\nmany blank lines\n\n\n",
    "html <b>bold</b> <script>x</script> &amp; entity &lt;tag&gt;",
    "".join(chr(0x20 + (i * 7919) % 95) for i in range(300)),
    "nerd font icons b 8 1 private-use glyphs",
    "wide punctuation ＡＢＣ１２３ ！？ fullwidth and ｱｲｳ halfwidth",
]

TOOL_OUTS = [
    ("Bash", '{"command":"ls -la /workspace"}', "total 12\ndrwxr-xr-x 2 node node 4096 .\n" + esc("32") + "colored" + esc("0") + " ls output\n"),
    ("Read", '{"file_path":"/workspace/x.go"}', "\n".join("%d\tline %d ends in m" % (i, i) for i in range(60))),
    ("Bash", '{"command":"jq -r"}', "\n".join("7d\t2026-08-27\tr=0.881\tz=1.84" for _ in range(5))),
    ("Bash", '{"command":"head -c 400 /dev/urandom"}', bytes((i * 131) % 256 for i in range(400)).decode("latin-1")),
]


def fixture_log():
    """The fixture transcript in the daemon's [[marker]] framing."""
    ts = int(time.time() * 1000) - 10 * 60 * 1000
    out = []
    turn = 0
    for body in FIXTURE_BODIES:
        turn += 1
        out += ["[[session]] -", "[ts:%d]" % (ts + turn * 1000), ">>> fixture turn %d: prompt with 😀 and 中文 and\ttab" % turn]
        out += ["[[think_begin]]"] + body.split("\n")[:3] + ["[[think_end]] 9"]
        if turn % 3 == 0:
            name, inp, res = TOOL_OUTS[turn % len(TOOL_OUTS)]
            lines = res.split("\n")
            out += ["[[tool]] %s %s" % (name, inp), "[[tool_out_begin]]"] + lines + ["[[tool_out_end]] %d" % len(lines)]
        out += body.split("\n")
        if turn % 5 == 0:
            out.append("[[err]] fixture error ends in m\twith tab")
        if turn % 7 == 0:
            out.append("[[notify]] %d high - fixture-title message body ends in m" % (ts + turn * 1000))
        out.append("[[turn_end]]")
    return "\n".join(out) + "\n"


# --- daemon plumbing (grpcurl in a container, like the verify skill) -------

def grpcurl(method, payload):
    token = open(os.path.join(HERE, "creds", "token-tui")).read().strip()
    cmd = ["podman", "run", "--rm", "-i", "--network", "koto-net", "--userns=keep-id", "--user", "1000",
           "--security-opt", "label=disable", "-v", HERE + "/creds:/creds:ro", "-v", HERE + "/protocol:/protocol:ro",
           "docker.io/fullstorydev/grpcurl:latest", "-cacert", "/creds/ca.crt", "-cert", "/creds/client-tui.crt",
           "-key", "/creds/client-tui.key", "-servername", "koto-daemon", "-H", "authorization: Bearer " + token,
           "-proto", "/protocol/koto.proto", "-import-path", "/protocol", "-d", payload,
           os.environ.get("KOTO_ADDR", "cs_host_go:8443"), "koto.Koto/" + method]
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0 or '"ok": true' not in r.stdout:
        sys.exit("tuiwalk: %s failed: %s %s" % (method, r.stdout.strip(), r.stderr.strip()))


# --- the emulator -----------------------------------------------------------

class Term(pyte.Screen):
    def __init__(self, cols, rows):
        super().__init__(cols, rows)
        self.in_draw = False
        self.events = []

    def draw(self, data):
        self.in_draw = True
        try:
            super().draw(data)
        finally:
            self.in_draw = False

    def linefeed(self):
        if self.in_draw:
            self.events.append(("WRAP", self.cursor.y, self.row_text(self.cursor.y)))
        super().linefeed()

    def index(self):
        bottom = self.margins.bottom if self.margins else self.lines - 1
        if self.cursor.y == bottom:
            self.events.append(("SCROLL", self.cursor.y, self.row_text(0)))
        super().index()

    def row_text(self, y):
        return "".join(self.buffer[y][x].data for x in range(self.columns)).rstrip()

    def dump(self):
        return "\n".join(self.row_text(y) for y in range(self.lines))


class Walker:
    UP, DOWN, PGUP, PGDN, HOME, END = b"\x1b[A", b"\x1b[B", b"\x1b[5~", b"\x1b[6~", b"\x1b[H", b"\x1b[F"
    ENTER, BS, DEL, TAB = b"\r", b"\x7f", b"\x1b[3~", b"\t"

    def __init__(self, run_dir, start_group, image):
        self.run = run_dir
        self.rows, self.cols = 24, 80
        self.screen = Term(self.cols, self.rows)
        self.stream = pyte.ByteStream(self.screen)
        self.carry = b""
        self.failures = []
        with open(os.path.join(run_dir, "tui-state.json"), "w") as f:
            f.write('{"cur":"%s"}' % start_group)
        token = open(os.path.join(HERE, "creds", "token-tui")).read().strip()
        self.m, s = pty.openpty()
        fcntl.ioctl(s, termios.TIOCSWINSZ, struct.pack("HHHH", self.rows, self.cols, 0, 0))
        self.name = "cs_tui_walk_%d" % os.getpid()

        def preexec():
            os.setsid()
            fcntl.ioctl(s, termios.TIOCSCTTY, 0)
        self.p = subprocess.Popen(
            ["podman", "run", "--rm", "-it", "--detach-keys=", "--name", self.name, "--network", "koto-net",
             "--security-opt", "label=disable", "-v", HERE + "/creds:/koto-creds:ro", "-v", run_dir + ":/koto-run",
             "-e", "TERM=xterm-256color", "-e", "COLORTERM=truecolor", "-e", "KOTO_TUI_TERM=xterm-kitty",
             "-e", "KOTO_TUI_NOTIFY=off", "-e", "KOTO_TOKEN=" + token,
             "-e", "KOTO_ENDPOINT=" + os.environ.get("KOTO_ADDR", "cs_host_go:8443"), image],
            stdin=s, stdout=s, stderr=s, preexec_fn=preexec)
        os.close(s)
        self.raw = open(os.path.join(run_dir, "walk.raw"), "wb")

    def close(self):
        self.send(self.HOME + self.DEL * 60 + self.END + self.BS * 60)
        self.send(b"/exit")
        self.pump(0.5)
        if self.input_row_is("/exit"):
            self.send(self.ENTER)
        self.pump(2)
        subprocess.run(["podman", "rm", "-f", self.name], capture_output=True)

    def pump(self, dur):
        end = time.time() + dur
        while True:
            left = end - time.time()
            if left <= 0:
                return
            r, _, _ = select.select([self.m], [], [], left)
            if not r:
                continue
            try:
                d = os.read(self.m, 65536)
            except OSError:
                return
            if not d:
                return
            self.raw.write(d)
            # xterm/kitty semantics: an ESC inside an escape restarts it, so
            # ESC ESC [0m is one SGR; pyte would print the "[0m". Fold the
            # pair before it looks, carrying a trailing ESC across chunks.
            d = self.carry + d
            self.carry = b""
            if d.endswith(b"\x1b"):
                self.carry, d = b"\x1b", d[:-1]
            while b"\x1b\x1b" in d:
                d = d.replace(b"\x1b\x1b", b"\x1b")
            self.stream.feed(d)
            if b"\x1b[6n" in d:  # cursor-position query: answer, or the TUI waits
                os.write(self.m, b"\x1b[%d;%dR" % (self.screen.cursor.y + 1, self.screen.cursor.x + 1))

    def send(self, b):
        os.write(self.m, b)

    def input_row_is(self, cmd):
        pat = re.compile(r"(^|│)\s*>\s+" + re.escape(cmd) + r"\s*(│|$)")
        lines = self.screen.dump().split("\n")
        hits = [ln for ln in lines[-10:] if pat.search(ln) and ln.count(cmd) == 1 and ">>>" not in ln]
        return len(hits) == 1

    def switch(self, g):
        """Submit `/sw g` only once the input row provably holds exactly that."""
        cmd = "/sw " + g
        for _ in range(3):
            self.send(self.HOME + self.DEL * 60 + self.END + self.BS * 60)
            self.pump(0.4)
            self.send(cmd.encode())
            tabbed = False
            for _ in range(15):
                self.pump(0.2)
                d = self.screen.dump()
                if "⇥/⎋ close" in d and not tabbed:  # tree focus: Enter would just close it
                    self.send(self.TAB)
                    tabbed = True
                    self.pump(0.3)
                    continue
                if self.input_row_is(cmd) and "⇥/⎋ tree" in d:
                    self.send(self.ENTER)
                    self.pump(1.5)
                    return True
        self.failures.append("could not stage %r on the input row" % cmd)
        return False

    def resize(self, r, c):
        self.rows, self.cols = r, c
        fcntl.ioctl(self.m, termios.TIOCSWINSZ, struct.pack("HHHH", r, c, 0, 0))
        self.screen.resize(r, c)
        os.killpg(self.p.pid, signal.SIGWINCH)
        self.pump(1.0)

    def drain(self, tag):
        ev = self.screen.events
        self.screen.events = []
        for kind, y, txt in ev:
            self.failures.append("[%s] %s at row %d: %r" % (tag, kind, y, txt[:160]))
        if ev:
            with open(os.path.join(self.run, "frame-%s.txt" % re.sub(r"[^\w.-]+", "_", tag)), "w") as f:
                f.write(self.screen.dump())
        return bool(ev)

    def walk(self, groups, sizes, pages):
        self.pump(5.0)
        self.screen.events = []
        for r, c in sizes:
            self.resize(r, c)
            self.drain("resize %dx%d" % (r, c))
            for g in groups:
                if not self.switch(g):
                    continue
                tag = "%s @%dx%d" % (g, c, r)
                self.drain(tag + " switch")
                for i in range(pages):
                    self.send(self.PGUP)
                    self.pump(0.25)
                    self.drain("%s pgup %d" % (tag, i))
                self.send(self.END)
                self.pump(0.3)
                self.drain(tag + " end")
            print("tuiwalk: %dx%d done, %d groups, failures so far: %d" % (c, r, len(groups), len(self.failures)), flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--all", action="store_true", help="also walk every existing group's real history (read-only)")
    ap.add_argument("--groups", default="", help="comma list of existing groups to walk instead of --all")
    ap.add_argument("--no-fixture", action="store_true", help="skip the fixture group")
    ap.add_argument("--sizes", default=DEFAULT_SIZES, help="ROWSxCOLS list (default %s)" % DEFAULT_SIZES)
    ap.add_argument("--pages", type=int, default=12, help="PgUp presses per group per size")
    ap.add_argument("--image", default="koto-tui", help="TUI image to drive")
    ap.add_argument("--keep", action="store_true", help="keep the fixture group and run dir afterwards")
    args = ap.parse_args()

    sizes = [tuple(int(x) for x in s.split("x")) for s in args.sizes.split(",")]
    groups = []
    if not args.no_fixture:
        groups.append(FIXTURE_GROUP)
    if args.groups:
        groups += args.groups.split(",")
    elif args.all:
        groups += sorted(g for g in os.listdir(os.path.join(HERE, "groups"))
                         if os.path.isdir(os.path.join(HERE, "groups", g, ".cs")) and g != FIXTURE_GROUP)
    if not groups:
        sys.exit("tuiwalk: nothing to walk (use the fixture, --all, or --groups)")

    spawned = False
    if not args.no_fixture:
        gdir = os.path.join(HERE, "groups", FIXTURE_GROUP)
        if os.path.exists(os.path.join(gdir, "prompt.md")):
            sys.exit("tuiwalk: refusing — groups/%s has a prompt.md, so it is not our fixture" % FIXTURE_GROUP)
        if not os.path.isdir(os.path.join(gdir, ".cs")):
            grpcurl("Spawn", '{"group":"%s"}' % FIXTURE_GROUP)
            spawned = True
        log = os.path.join(gdir, ".cs", "log")
        with open(log, "a", encoding="utf-8", errors="surrogateescape") as f:
            f.write(fixture_log())

    run_dir = tempfile.mkdtemp(prefix="tuiwalk-")
    w = Walker(run_dir, groups[0], args.image)
    try:
        w.walk(groups, sizes, args.pages)
    finally:
        w.close()
        if spawned and not args.keep:
            grpcurl("Destroy", '{"group":"%s"}' % FIXTURE_GROUP)

    if w.failures:
        print("tuiwalk: FAIL — %d finding(s); frames in %s" % (len(w.failures), run_dir))
        for f in w.failures:
            print("  " + f)
        sys.exit(1)
    print("tuiwalk: OK — %d group(s) x %d size(s), no wraps, no scrolls" % (len(groups), len(sizes)))
    if not args.keep:
        subprocess.run(["rm", "-rf", run_dir])


if __name__ == "__main__":
    main()
