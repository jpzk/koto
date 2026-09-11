package main

// Self-bootstrapped user namespace — what rootless podman used to give the
// daemon for free, now that the daemon runs directly on the host.
//
// Two things in the daemon need to be root over a RANGE of ids, not just over
// their own uid:
//
//   - fcjail writes a two-entry uid_map for each VMM (`0→0` plus the per-VM
//     id). An unprivileged process may write only one entry, mapping its own
//     euid, so this fails with EPERM outside a userns.
//   - fcJailFixupPerms chowns each group's socket dir and workspace image to
//     that per-VM id. Unprivileged chown to another uid is EPERM.
//
// Under podman both worked because the container had already placed the daemon
// inside a userns as uid 0 owning 65536 subuids. We now do the same thing
// ourselves, once, at startup: re-exec into a fresh userns and have
// newuidmap/newgidmap install the mapping from /etc/subuid and /etc/subgid.
// Those helpers carry cap_setuid/cap_setgid, so nothing here needs root — the
// administrator's one-time act is allocating the range, which rootless podman
// required anyway.
//
// After the bootstrap the daemon is in exactly the shape it had inside the
// container — ns-root with a mapped subuid range — so every downstream caller
// is untouched. That is the point: this file replaces podman, not the jail.

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"
)

// usernsEnv tracks how far the bootstrap has got. Three stages are needed,
// and the third is not obvious:
//
//	unset -> the original process; clones a child into a fresh userns
//	"1"   -> that child, still UNMAPPED at exec time and therefore holding no
//	         capabilities; waits for the mapping, then re-execs itself
//	"2"   -> the real daemon: exec'd while already uid 0 in the namespace, so
//	         the kernel grants the capability set that CAP_CHOWN and the
//	         jailer's uid_map writes depend on
//
// Stage 2 exists because Go's os/exec clones and execs in one step: the child
// is exec'd before the parent can write /proc/<pid>/uid_map, so it execs as
// the overflow uid with an empty permitted set. Writing the map afterwards
// changes its uid to 0 but does NOT retroactively grant capabilities — the
// process reads as root and still cannot chown. Verified the hard way with
// `koto userns-check`. Exec'ing once more, now that euid is 0, fixes it.
const usernsEnv = "KOTO_USERNS_READY"

// usernsSyncFD is the pipe the child blocks on until the parent has installed
// the id mappings. Without it the child would race ahead as the overflow uid.
const usernsSyncFD = 3

// subIDRange parses /etc/subuid or /etc/subgid for the given user, returning
// the first range allocated to them. Lines are "name:start:count", and either
// the username or the numeric id may appear as the key.
func subIDRange(path, username, id string) (start, count int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) != 3 || (f[0] != username && f[0] != id) {
			continue
		}
		s, err1 := strconv.Atoi(f[1])
		c, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || c <= 0 {
			continue
		}
		return s, c, nil
	}
	return 0, 0, fmt.Errorf("%s has no range for %s — allocate one with\n"+
		"  sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s",
		path, username, username)
}

// usernsNeeded reports whether we must bootstrap. Already-in-a-userns (euid 0
// with the marker set) means the parent did it; running as real root means
// there is nothing to gain and the mappings would be meaningless.
func usernsNeeded() bool {
	if os.Getenv(usernsEnv) != "" {
		return false
	}
	return os.Geteuid() != 0
}

// usernsReady reports whether the bootstrap has fully completed, i.e. we are
// the stage-2 process holding capabilities in the namespace.
func usernsReady() bool { return os.Getenv(usernsEnv) == "2" }

// usernsBootstrap re-execs the daemon inside a user namespace with the host's
// subuid/subgid range mapped, then supervises it. It does not return in the
// parent: the parent's only remaining job is to forward signals and exit with
// the child's status.
//
// The parent has to survive because the mapping can only be written from
// OUTSIDE the new namespace, which rules out a plain unshare+exec in one
// process. It is the same shape as rootless podman's pause process.
func usernsBootstrap() error {
	me, err := user.Current()
	if err != nil {
		return err
	}
	uidStart, uidCount, err := subIDRange("/etc/subuid", me.Username, me.Uid)
	if err != nil {
		return err
	}
	gidStart, gidCount, err := subIDRange("/etc/subgid", me.Username, me.Gid)
	if err != nil {
		return err
	}
	// The per-VM band (fcJailUID) must fit inside what we were allocated,
	// or VMs would silently collide on a clamped id.
	if uidCount <= fcJailBaseUID || gidCount <= fcJailBaseUID {
		return fmt.Errorf("subuid range for %s is %d ids, need more than %d "+
			"(the per-VM jail band starts there)", me.Username, uidCount, fcJailBaseUID)
	}
	// ...and the check above only proves the band STARTS inside the allocation
	// (audit 2026-09-11 L89). The band runs to fcJailMaxUID, so an allocation
	// of, say, 32768 ids passed while leaving 32768..60000 unmapped — and an
	// id from the unmapped part is not a usable identity: fcJailFixupPerms
	// chowns with it and fcJailCommand writes it as a nested mapping's HostID,
	// both of which fail in ways that read as a broken boot rather than as a
	// misconfiguration.
	//
	// Refusing to start would be the wrong answer: with a short allocation the
	// LOW part of the band is perfectly usable, and a fleet already running on
	// it must not be taken down by an upgrade. So the band is CLAMPED to what
	// was actually allocated, and a group whose port falls past the clamp is
	// refused by name at boot (fcJailUID) with the range in the message.
	if max := min(uidCount, gidCount) - 1; max < fcJailMaxUID {
		jailMaxUID = max
		emitLogf("fc", "warn", "subuid/subgid range for %s covers only %d ids: the per-VM jail band is clamped to %d-%d "+
			"(the full band is %d-%d). Groups whose proxy port maps past %d will refuse to boot; "+
			"widen the range in /etc/subuid and /etc/subgid to lift this.",
			me.Username, min(uidCount, gidCount), fcJailBaseUID, jailMaxUID, fcJailBaseUID, fcJailMaxUID, jailMaxUID)
	}

	newuidmap, err := exec.LookPath("newuidmap")
	if err != nil {
		return fmt.Errorf("newuidmap not found — install shadow-utils (Fedora) or uidmap (Debian/Ubuntu): %w", err)
	}
	newgidmap, err := exec.LookPath("newgidmap")
	if err != nil {
		return fmt.Errorf("newgidmap not found — install shadow-utils (Fedora) or uidmap (Debian/Ubuntu): %w", err)
	}

	// The child blocks on this pipe until the mappings are in place.
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Env = append(os.Environ(), usernsEnv+"=1", jailMaxEnv+"="+strconv.Itoa(jailMaxUID))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{pr} // becomes fd 3 in the child
	// No Uid/GidMappings here on purpose: Go would write them directly, which
	// is exactly the unprivileged single-entry write that cannot express what
	// we need. newuidmap does it instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return fmt.Errorf("re-exec into userns: %w", err)
	}
	pr.Close()
	pid := strconv.Itoa(cmd.Process.Pid)

	// ns id 0 → our real uid, so files we create are still ours on disk.
	// ns ids 1..count → the allocated subuid range, which is where the
	// per-VM jail band lives.
	mapArgs := func(nsSelf string, start, count int) []string {
		return []string{pid, "0", nsSelf, "1", "1", strconv.Itoa(start), strconv.Itoa(count)}
	}
	if out, err := exec.Command(newuidmap, mapArgs(me.Uid, uidStart, uidCount)...).CombinedOutput(); err != nil {
		cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("newuidmap: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command(newgidmap, mapArgs(me.Gid, gidStart, gidCount)...).CombinedOutput(); err != nil {
		cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("newgidmap: %v: %s", err, strings.TrimSpace(string(out)))
	}

	// Release the child.
	if _, err := pw.Write([]byte{1}); err != nil {
		cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return fmt.Errorf("release child: %w", err)
	}
	pw.Close()

	// Forward termination so systemd's SIGTERM still reaches the real daemon,
	// which is what lets it stop every microVM and leave the workspace images
	// clean. Without this the supervisor would die and orphan the fleet.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for s := range sigs {
			if p := cmd.Process; p != nil {
				_ = p.Signal(s)
			}
		}
	}()

	err = cmd.Wait()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "koto: userns child: %v\n", err)
		code = 1
	}
	os.Exit(code)
	return nil // unreachable
}

// usernsAwaitParent is the stage-1 child: block until the parent has installed
// the id mappings, then re-exec so the kernel grants capabilities to what is
// now a uid-0 process. It does not return on success.
func usernsAwaitParent() error {
	f := os.NewFile(usernsSyncFD, "koto-userns-sync")
	if f == nil {
		return fmt.Errorf("userns sync pipe missing")
	}
	buf := make([]byte, 1)
	if _, err := f.Read(buf); err != nil {
		f.Close()
		return fmt.Errorf("userns sync: %w", err)
	}
	f.Close()
	if os.Geteuid() != 0 {
		return fmt.Errorf("userns mapping did not take effect (euid %d, want 0)", os.Geteuid())
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	// Replace, don't append: os.Environ() still carries the stage-1 value and
	// execve keeps the FIRST occurrence of a duplicated key, so appending
	// would leave us reading "1" forever and looping on the sync pipe.
	env := setEnv(os.Environ(), usernsEnv, "2")
	env = setEnv(env, jailMaxEnv, strconv.Itoa(jailMaxUID))
	// Exec, not fork: this process becomes the daemon, so the supervising
	// parent's Wait() and signal forwarding keep working unchanged.
	if err := syscall.Exec(self, os.Args, env); err != nil {
		return fmt.Errorf("userns stage-2 exec: %w", err)
	}
	return nil // unreachable
}

// usernsProbeMain implements `koto userns-check`: run the real bootstrap, then
// assert the two privileges the jailer depends on — being ns-root, and being
// able to chown into the per-VM band. It is the honest end-to-end answer to
// "can this host run jailed microVMs without a container", and it is what the
// integration test drives.
func usernsProbeMain() {
	if err := usernsEnsure(); err != nil {
		ctlFatal(1, "userns: %v", err)
	}
	dir, err := os.MkdirTemp("", "koto-userns")
	if err != nil {
		ctlFatal(1, "%v", err)
	}
	defer os.RemoveAll(dir)
	f := dir + "/probe"
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		ctlFatal(1, "%v", err)
	}
	if err := os.Chown(f, fcJailBaseUID, fcJailBaseUID); err != nil {
		ctlFatal(1, "chown into the jail band failed: %v", err)
	}
	fmt.Printf("userns ok: euid=%d, chown to %d works — jailed microVMs are supported\n",
		os.Geteuid(), fcJailBaseUID)
}

// usernsEnsure is the single entry point callers use: bootstrap if needed,
// finish the handshake if we are the stage-1 child, no-op once ready.
func usernsEnsure() error {
	switch {
	case usernsReady():
		return nil
	case usernsNeeded():
		return usernsBootstrap() // exits with the child's status
	default:
		return usernsAwaitParent() // execs stage 2
	}
}

// setEnv returns env with key set to val, replacing any existing entry.
func setEnv(env []string, key, val string) []string {
	out := env[:0:0]
	for _, e := range env {
		if k, _, ok := strings.Cut(e, "="); !ok || k != key {
			out = append(out, e)
		}
	}
	return append(out, key+"="+val)
}
