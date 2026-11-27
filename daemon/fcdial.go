package main

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// ---- verified host→VMM dial (audit p02-001) --------------------------------
//
// The per-group sockdir run/fc/<g>.sock is owned by that group's jail uid and
// bind-mounted read-write into its VMM chroot, so a VMM that escaped its guest
// can unlink its "v" socket and put a symlink there (text "../main.sock/v"
// need not resolve inside the chroot). A plain pathname connect from the
// daemon — ns-root with CAP_DAC_OVERRIDE over the jail subuids — would follow
// that link into ANOTHER group's 0700 sockdir and hand the hybrid-vsock
// CONNECT to that group's guest agent. Every host→guest connect therefore goes
// through fcDialVMM, which never resolves the VMM-writable name by path:
//
//  1. open the sockdir O_PATH|O_DIRECTORY|O_NOFOLLOW (its parent run/fc is
//     daemon-owned 0700, so the VMM cannot replace the dir itself; NOFOLLOW
//     still refuses a symlinked dir outright);
//  2. openat(dirfd, "v", O_PATH|O_NOFOLLOW) — pins the INODE that is there
//     now, without following a link: a symlink yields the link's own inode;
//  3. fstat that fd: it must be a socket owned by this group's jail uid;
//  4. connect through /proc/self/fd/<n>, which reaches exactly the pinned
//     inode no matter what the VMM renames/unlinks/symlinks afterwards;
//  5. SO_PEERCRED on the CONNECTED socket: the listener's uid must be this
//     group's jail uid (belt and braces against anything that got past 3,
//     e.g. a hard link to a foreign socket inode).
//
// There is no check-then-use window: steps 3-5 all look at the object we
// actually connect to (the pinned inode, then the connected peer), never at a
// name that is re-resolved later. A foreign socket is refused at step 3
// before any connect(2), so the other group's VMM never even sees an accept.

// fcDialUIDs holds each group's jail uid as seen in the daemon's user
// namespace — the very value fcSpawn computes with fcJailUID and hands to
// fcJailFixupPerms / the jail spec. fcJailCommand maps spec.UID to itself, so
// the socket inode Firecracker creates and its SO_PEERCRED both report this
// uid here. Registered at spawn before the first dial; a group with no entry
// is refused (fail closed).
var (
	fcDialUIDMu sync.Mutex
	fcDialUIDs  = map[string]int{}
)

func fcSetDialUID(g string, uid int) {
	fcDialUIDMu.Lock()
	fcDialUIDs[g] = uid
	fcDialUIDMu.Unlock()
}

func fcDialUID(g string) (int, bool) {
	fcDialUIDMu.Lock()
	defer fcDialUIDMu.Unlock()
	uid, ok := fcDialUIDs[g]
	return uid, ok
}

// fcPeerUID reads the connected peer's uid. A var only so a test can stand in
// a foreign uid without root (both test listeners share the test's uid).
var fcPeerUID = func(c *net.UnixConn) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var cerr error
	if err := rc.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return int(cred.Uid), nil
}

// fcDialRefused logs the refusal against the group and returns the error.
func fcDialRefused(g, why string) error {
	emitLogfG("fc", g, "warn", "[%s] refusing host dial of VMM socket %s: %s (possible VMM escape, audit p02-001)", g, fcUDS(g), why)
	return fmt.Errorf("vmm socket for group %s refused: %s", g, why)
}

// fcDialVMM connects to group g's Firecracker hybrid-vsock base socket
// (fcUDS) after verifying, race-free, that it is that group's own socket.
// The ONLY place the daemon connects to a group VMM socket.
func fcDialVMM(g string, timeout time.Duration) (*net.UnixConn, error) {
	want, ok := fcDialUID(g)
	if !ok {
		return nil, fcDialRefused(g, "no jail uid registered for this group")
	}
	dfd, err := unix.Open(fcSockDir(g), unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if err == unix.ELOOP || err == unix.ENOTDIR {
			return nil, fcDialRefused(g, "sockdir is not a directory")
		}
		return nil, fmt.Errorf("open vmm sockdir: %w", err)
	}
	fd, err := unix.Openat(dfd, "v", unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	unix.Close(dfd)
	if err != nil {
		return nil, fmt.Errorf("open vmm socket: %w", err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("stat vmm socket: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return nil, fcDialRefused(g, fmt.Sprintf("v is not a socket (mode %#o)", st.Mode&unix.S_IFMT))
	}
	if int(st.Uid) != want {
		return nil, fcDialRefused(g, fmt.Sprintf("v owned by uid %d, want jail uid %d", st.Uid, want))
	}
	// connect(2) on the /proc magic link reaches the pinned inode itself.
	c, err := net.DialTimeout("unix", "/proc/self/fd/"+strconv.Itoa(fd), timeout)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return nil, fcDialRefused(g, "not a unix conn")
	}
	peer, err := fcPeerUID(uc)
	if err != nil {
		uc.Close()
		return nil, fcDialRefused(g, fmt.Sprintf("SO_PEERCRED: %v", err))
	}
	if peer != want {
		uc.Close()
		return nil, fcDialRefused(g, fmt.Sprintf("peer uid %d, want jail uid %d", peer, want))
	}
	return uc, nil
}

