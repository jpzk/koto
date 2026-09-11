package main

// `koto uninstall` — the inverse of the integrate stage, and only of that
// stage. It removes what `koto install` put on the system: the systemd unit,
// the binaries on PATH, and (with --purge) /etc/koto and the state directory.
//
// THE SPLIT IS dpkg's, deliberately. A bare `koto uninstall` removes the
// integration and keeps every byte of your data — group workspaces, the CA
// and client identities, schedules, goals, the guest assets. `--purge` is the
// separate, prompted verb that deletes them. The project already made this
// call once for `make clean` (safe) vs `make clean-groups` (destructive,
// prompted): an operator uninstalling a service is saying "stop running
// this", which is not the same sentence as "destroy my agents' history", and
// conflating the two makes the safe action unavailable.
//
// The ORDERING is load-bearing at exactly one point: the service is stopped
// FIRST, before the unit or the binary goes anywhere. A microVM's workspace
// is an ext4 image the guest has mounted, and the daemon's SIGTERM handler is
// what gives each guest its ~12s to sync and unmount (TimeoutStopSec=25 in
// the unit). Removing /usr/local/bin/koto or the unit first would not kill
// the running daemon — but a later `systemctl stop` would have no unit to
// stop, and the fleet would go down with the host instead, every image dirty.
//
// Like the wizard, it works by DETECTION rather than by a manifest: each
// thing is removed if it is there and skipped if it is not, so an interrupted
// uninstall finishes by re-running, and a half-installed system (the shape a
// failed install leaves) cleans up the same way.

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type uninstallOpts struct {
	stateDir string
	purge    bool
	dry      bool
	ui       *setupUI
}

// step runs one privileged command, or, under --dry-run, only says it would.
// Every mutation in this file goes through it or through the dry check beside
// os.RemoveAll, so --dry-run is a property of the command rather than a claim.
func (o uninstallOpts) step(args ...string) error {
	if o.dry {
		o.ui.info("%s", o.ui.dim("would: sudo "+strings.Join(args, " ")))
		return nil
	}
	return sudoRun(o.ui, args...)
}

func uninstallUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto uninstall [flags]

Removes the systemd service, the unit file and the koto binaries. Stops the
daemon first, so every running microVM syncs and unmounts its workspace
image cleanly.

Your data is NOT touched: the state directory (group workspaces, creds,
schedules, goals, guest assets) and /etc/koto/koto.env are left in place, and
a later 'koto install' picks them up exactly where they were. Pass --purge to
delete them too — that is prompted, and it is not reversible.

flags:
  -state DIR   state directory (default /var/lib/koto)
  --purge      also delete the state directory and /etc/koto
  -n           dry run: print what would be removed, change nothing
  -y           accept defaults, never prompt (with --purge: delete unprompted)`)
}

func uninstallMain(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	fs.Usage = uninstallUsage
	state := fs.String("state", defaultStateDir, "state directory")
	purge := fs.Bool("purge", false, "also delete the state directory")
	dry := fs.Bool("n", false, "dry run")
	assumeYes := fs.Bool("y", false, "accept defaults")
	noColor := fs.Bool("no-color", false, "disable color")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		uninstallFatal(2, "unexpected argument %q — this command takes flags only", fs.Arg(0))
	}
	if err := runUninstall(uninstallOpts{
		stateDir: *state, purge: *purge, dry: *dry, ui: newSetupUI(*assumeYes, *noColor),
	}); err != nil {
		uninstallFatal(1, "%v", err)
	}
}

// uninstallFatal prefixes with the command the operator actually typed, the
// way claudeLoginFatal does. ctlFatal would say "koto ctl:" here, which names
// a subcommand that is not running.
func uninstallFatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "koto uninstall: "+format+"\n", a...)
	os.Exit(code)
}

func runUninstall(o uninstallOpts) error {
	u := o.ui
	me, err := user.Current()
	if err != nil {
		return err
	}
	// Same refusal as install, for the same reason: the state dir is owned by
	// the invoking user, and running this as root would make the ownership
	// checks below answer about the wrong person.
	if me.Uid == "0" {
		return fmt.Errorf("run as your normal user, not root (sudo is used only for the system files)")
	}
	state, err := filepath.Abs(o.stateDir)
	if err != nil {
		return err
	}

	if o.dry {
		u.info("%s", u.bold("dry run — nothing will be changed"))
	}
	present := exists(unitPath) || exists("/usr/local/bin/koto") ||
		exists("/usr/local/bin/koto-tui") || exists(envFilePath)
	if !present {
		u.info("nothing installed — no unit, no binaries, no %s", envFilePath)
		if !o.purge {
			return nil
		}
	}

	// 1. Stop, before anything is removed. This is the step that lets every
	// guest sync and unmount; the rest is just files.
	//
	// Whether to stop used to be decided by `exists(unitPath)` ALONE (audit
	// 2026-09-11 L54), which is a question about a file, not about whether a
	// daemon is running. With the unit gone — removed by hand, an interrupted
	// earlier uninstall, a daemon started straight from a binary — the stop
	// was skipped silently and the run went on to delete the binaries and,
	// under --purge, the state directory out from under a LIVE daemon and its
	// live microVMs: every workspace image dirty, which is precisely the
	// failure the ordering at the top of this file exists to prevent.
	//
	// So the unit file is only one of three signals. systemd may still hold
	// the unit loaded after the file is gone (no daemon-reload), and a daemon
	// may be running with no unit at all.
	unitActive := systemctlIsActive("koto")
	if exists(unitPath) || unitActive {
		u.info("stopping the daemon — each guest gets up to 25s to unmount its workspace")
		if err := o.step("systemctl", "stop", "koto"); err != nil {
			// A stop that fails leaves VMs running against a service we are
			// about to delete, which is the one state worth refusing from.
			return fmt.Errorf("stop koto: %w (fix, or `sudo systemctl kill koto`, then re-run)", err)
		}
		// disable/reset-failed are best-effort: a unit that was never enabled
		// makes disable exit non-zero, and that is not a failure of anything.
		_ = o.step("systemctl", "disable", "koto")
		if err := o.step("rm", "-f", unitPath); err != nil {
			return fmt.Errorf("remove unit: %w", err)
		}
		if err := o.step("systemctl", "daemon-reload"); err != nil {
			return err
		}
		_ = o.step("systemctl", "reset-failed", "koto")
		if !o.dry {
			u.ok("service stopped and removed")
		}
	}

	// Post-stop verification: `systemctl stop` succeeding is not the same
	// sentence as "no daemon is serving this state dir". Under --dry-run
	// nothing was stopped, so this reports rather than refuses.
	if pid, cmd, found := runningKotoDaemon(state); found {
		if o.dry {
			u.warn("a koto daemon is running against %s (pid %d: %s) — "+
				"a real run would refuse until it is stopped", state, pid, cmd)
		} else {
			return fmt.Errorf("a koto daemon is still running against %s (pid %d: %s).\n"+
				"Removing files under a live daemon leaves every guest's workspace image dirty — "+
				"each one needs the daemon's SIGTERM handler to sync and unmount.\n"+
				"Stop it first (`sudo systemctl stop koto`, or `kill %d` for a daemon started by hand), then re-run",
				state, pid, cmd, pid)
		}
	}

	// 2. The binaries. Unlinking the running binary is fine on Linux — this
	// process is already mapped — so `koto uninstall` can remove itself.
	var bins []string
	for _, b := range []string{"/usr/local/bin/koto", "/usr/local/bin/koto-tui"} {
		if exists(b) {
			bins = append(bins, b)
		}
	}
	if len(bins) > 0 {
		if err := o.step(append([]string{"rm", "-f"}, bins...)...); err != nil {
			return fmt.Errorf("remove binaries: %w", err)
		}
		if !o.dry {
			u.ok("removed %s", strings.Join(bins, ", "))
		}
	}

	if !o.purge {
		u.blank()
		u.info("kept, and a later `koto install` will pick both up:")
		if exists(envFilePath) {
			u.info("  %s — may hold your ANTHROPIC_API_KEY", envFilePath)
		}
		if exists(state) {
			u.info("  %s — %s", state, uninstallStateSummary(state))
		}
		u.info("delete them with `koto uninstall --purge`")
		return nil
	}
	return uninstallPurge(o, state)
}

// systemctlIsActive answers whether systemd currently has the unit running.
// Read-only, so it needs no sudo, and a missing systemctl (or a unit systemd
// has never heard of) is simply "not active" — the caller has two other
// signals.
func systemctlIsActive(unit string) bool {
	out, err := exec.Command("systemctl", "is-active", unit).Output()
	if err != nil && len(out) == 0 {
		return false
	}
	return strings.TrimSpace(string(out)) == "active"
}

// runningKotoDaemon looks for a live `koto daemon` process serving the given
// state directory. It reads /proc directly rather than shelling out to pgrep:
// no dependency, and — the part that matters — it can tell WHICH state dir a
// daemon is serving, so a dev-clone daemon under .dev/ does not block the
// uninstall of an installed one, and vice versa.
//
// A daemon resolves its state root exactly as kotoHome() does: KOTO_HOME when
// set, otherwise its cwd. Both are readable from /proc for our own processes;
// when they are not (another user's daemon), the process is reported anyway —
// refusing on a daemon that might not be ours is the safe direction, and the
// message names the pid so the operator can judge.
func runningKotoDaemon(state string) (int, string, bool) {
	want := resolvePath(state)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, "", false
	}
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // gone, or not ours to read
		}
		argv := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		if len(argv) < 2 || filepath.Base(argv[0]) != "koto" || argv[1] != "daemon" {
			continue
		}
		home, known := procKotoHome(pid)
		if known && resolvePath(home) != want {
			continue // a different koto (a dev clone, another state dir)
		}
		return pid, strings.Join(argv, " "), true
	}
	return 0, "", false
}

// procKotoHome reports the state root of a running daemon, and whether it
// could be determined at all.
func procKotoHome(pid int) (string, bool) {
	env, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err == nil {
		for _, kv := range strings.Split(strings.TrimRight(string(env), "\x00"), "\x00") {
			if v, ok := strings.CutPrefix(kv, "KOTO_HOME="); ok && v != "" {
				return v, true
			}
		}
	}
	// No KOTO_HOME: the daemon resolved its root from its cwd.
	cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", false
	}
	return cwd, true
}

// resolvePath canonicalises for comparison, falling back to the cleaned
// absolute form when the path cannot be resolved (it may not exist yet).
func resolvePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}

// uninstallPurge deletes the state dir and /etc/koto, behind the guards that
// make an irreversible `rm -rf` on an operator-supplied path defensible.
func uninstallPurge(o uninstallOpts, state string) error {
	u := o.ui
	if !exists(state) {
		u.info("%s does not exist — nothing to purge", state)
	} else {
		// Identity, taken BEFORE the guards and re-checked after the prompt.
		// purgeRefusal works on a PATHNAME, and RemoveAll resolves that name
		// again — with a custom -state under a writable parent, the parent can
		// be renamed and replaced with a symlink in between, so the
		// confirmation shows one directory and the deletion walks another
		// (audit M112). The prompt is the wide part of that window: it waits
		// on a human.
		stateID, idErr := dirIdentity(state)
		if idErr != nil {
			return fmt.Errorf("stat %s: %w", state, idErr)
		}
		if why := purgeRefusal(state); why != "" {
			return fmt.Errorf("refusing to delete %s: %s", state, why)
		}
		u.blank()
		u.warn("about to DELETE %s (%s)", state, uninstallStateSummary(state))
		u.info("that is every group workspace and conversation, the CA and all client")
		u.info("identities, schedules, goals and the guest assets. There is no undo.")
		// -y means yes HERE only because --purge was also typed: the flag
		// alone can never delete anything, and asking for the purge is
		// itself the deliberate act (apt's `purge -y`, same bargain). The
		// prompt's own default stays no, so a bare --purge on a pipe with
		// nothing to answer it keeps the data.
		switch {
		case o.dry:
			u.info("%s", u.dim("would: rm -rf "+state))
		case u.yes:
			u.info("-y with --purge — deleting without prompting")
		case !u.yesno("Delete it?", false):
			u.info("kept %s", state)
			return nil
		}
		// The state dir is owned by the invoking user, so no sudo: if this
		// needs root, the path is not a koto state dir and the guards above
		// were the wrong ones.
		if !o.dry {
			// The same directory the guards passed and the operator saw, or
			// nothing: an rm -rf on a path that changed identity under a
			// confirmation prompt is exactly what this must not do.
			if now, err := dirIdentity(state); err != nil {
				return fmt.Errorf("stat %s before deleting: %w", state, err)
			} else if now != stateID {
				return fmt.Errorf("%s is not the directory that was checked "+
					"(it was replaced while the confirmation was open) — refusing to delete it", state)
			}
			if err := os.RemoveAll(state); err != nil {
				return fmt.Errorf("remove %s: %w", state, err)
			}
			u.ok("deleted %s", state)
		}
	}
	if exists(envFilePath) {
		if err := o.step("rm", "-f", envFilePath); err != nil {
			return fmt.Errorf("remove %s: %w", envFilePath, err)
		}
		// Only if empty: /etc/koto is ours, but a directory someone else put
		// something in is not a thing to take with us.
		_ = o.step("rmdir", "--ignore-fail-on-non-empty", filepath.Dir(envFilePath))
		if !o.dry {
			u.ok("removed %s", envFilePath)
		}
	}
	return nil
}

// dirIdentity is a directory's (device, inode) pair — stable across renames of
// the path that names it, and different for any other directory. Lstat, not
// Stat: a symlink swapped in for the validated directory must read as a
// different object, not as whatever it points at.
func dirIdentity(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("cannot read the identity of %s", path)
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino), nil
}

// purgeRefusal returns why a path must not be deleted, or "" when it may be.
// Every check exists because --purge is an `rm -rf` on a path the operator
// typed, and `-state` one directory too high is an ordinary typo.
func purgeRefusal(dir string) string {
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return "not an absolute path"
	}
	if dir == "/" || filepath.Dir(dir) == dir {
		return "that is the filesystem root"
	}
	if home, err := os.UserHomeDir(); err == nil {
		// Resolve both sides: a symlinked home compared lexically is a miss
		// (audit L10).
		rh, rd := home, dir
		if r, err := filepath.EvalSymlinks(home); err == nil {
			rh = r
		}
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			rd = r
		}
		if filepath.Clean(rh) == filepath.Clean(rd) || filepath.Clean(home) == dir {
			return "that is your home directory"
		}
	}
	// A clone is not a state dir. They have the same subdirectories by
	// design (that is the whole KOTO_HOME trick), so "looks like koto" alone
	// would happily delete the checkout you are standing in.
	if exists(filepath.Join(dir, ".git")) || exists(filepath.Join(dir, "go.work")) {
		return "that is a koto clone, not an installed state dir"
	}
	// Last: it must actually look like one. An empty or unrelated directory
	// means -state named the wrong place, and deleting it would be silent.
	// Last: it must actually look like one. An empty or unrelated directory
	// means -state named the wrong place, and deleting it would be silent.
	// Any ONE marker is enough, deliberately: the stamp is written at the
	// END of `koto install`, so a half-installed system has only some of
	// these, and refusing to clean it up would strand it (the 2026-09-04
	// audit's L10 asks for the stamp alone; that trade is the operator's).
	for _, marker := range []string{".koto-version", "groups", "creds", "fcassets", "groups.json"} {
		if exists(filepath.Join(dir, marker)) {
			return ""
		}
	}
	return "no koto state found there (no groups/, creds/, fcassets/ or .koto-version)"
}

// uninstallStateSummary describes a state dir in one clause — the group count
// and its size on disk — so the confirmation prompt says what is at stake
// instead of only where it lives.
func uninstallStateSummary(state string) string {
	n := 0
	if entries, err := os.ReadDir(filepath.Join(state, "groups")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				n++
			}
		}
	}
	groups := fmt.Sprintf("%d groups", n)
	if n == 1 {
		groups = "1 group"
	}
	// `du` rather than a walk of our own: it is sparse-aware, and the number
	// an operator can check afterwards should come from the tool they would
	// check it with.
	out, err := exec.Command("du", "-sh", state).Output()
	if err != nil {
		return groups
	}
	if f := strings.Fields(string(out)); len(f) > 0 {
		return groups + ", " + f[0]
	}
	return groups
}
