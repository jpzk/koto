package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// The host grows a group's disk by extending the sparse backing file only; it
// deliberately runs no filesystem tool on the guest-authored image. Growing
// the ext4 filesystem into the new space happens here, online, after the
// workspace is mounted: ext4 supports growing a mounted filesystem, and doing
// it in the guest keeps every parse of this metadata inside the VM.

// wsGrowSlack is how much larger the device must be than the filesystem before
// a resize is attempted. mke2fs may leave a partial last block group unused,
// and size presets move in GiB steps, so this only skips the no-op case.
const wsGrowSlack = 64 << 20

// wsGrowTimeout bounds the online resize so a slow or wedged resize2fs can
// never hold up boot.
const wsGrowTimeout = 90 * time.Second

// ext4FSBytes returns the filesystem size recorded in an ext2/3/4 primary
// superblock (the 1024 bytes at device offset 1024).
func ext4FSBytes(sb []byte) (int64, error) {
	if len(sb) < 0x160 {
		return 0, errors.New("short superblock")
	}
	if binary.LittleEndian.Uint16(sb[0x38:]) != 0xEF53 {
		return 0, errors.New("no ext4 superblock magic")
	}
	logBS := binary.LittleEndian.Uint32(sb[0x18:])
	if logBS > 6 { // 1 KiB << 6 = 64 KiB, the ext4 maximum
		return 0, fmt.Errorf("implausible block size log %d", logBS)
	}
	blocks := uint64(binary.LittleEndian.Uint32(sb[0x04:]))
	const incompat64bit = 0x80
	if binary.LittleEndian.Uint32(sb[0x60:])&incompat64bit != 0 {
		blocks |= uint64(binary.LittleEndian.Uint32(sb[0x150:])) << 32
	}
	bs := uint64(1024) << logBS
	if blocks == 0 || blocks > (1<<62)/bs {
		return 0, fmt.Errorf("implausible block count %d", blocks)
	}
	return int64(blocks * bs), nil
}

// workspaceNeedsGrow is the resize decision: only grow, and only when the
// device has meaningfully more room than the filesystem uses.
func workspaceNeedsGrow(fsBytes, devBytes int64) bool {
	return fsBytes > 0 && devBytes > fsBytes && devBytes-fsBytes >= wsGrowSlack
}

// devSizes reads the filesystem size from dev's superblock and the device
// size by seeking to its end (works for block devices and regular files).
func devSizes(dev string) (fsBytes, devBytes int64, err error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	sb := make([]byte, 1024)
	if _, err := f.ReadAt(sb, 1024); err != nil {
		return 0, 0, fmt.Errorf("read superblock: %w", err)
	}
	if fsBytes, err = ext4FSBytes(sb); err != nil {
		return 0, 0, err
	}
	if devBytes, err = f.Seek(0, io.SeekEnd); err != nil {
		return 0, 0, fmt.Errorf("device size: %w", err)
	}
	return fsBytes, devBytes, nil
}

// wsResizeCmd is the exec seam for tests.
var wsResizeCmd = func(ctx context.Context, dev string) ([]byte, error) {
	return exec.CommandContext(ctx, "resize2fs", dev).CombinedOutput()
}

// growWorkspace grows the mounted workspace filesystem to fill dev when the
// host extended the disk. It never fails boot: every problem is logged and
// the workspace stays usable at its old size. Returns whether a resize was
// attempted and succeeded (for tests).
func growWorkspace(dev string) bool {
	fsBytes, devBytes, err := devSizes(dev)
	if err != nil {
		logf("workspace grow: cannot size %s, skipping: %v", dev, err)
		return false
	}
	if !workspaceNeedsGrow(fsBytes, devBytes) {
		return false
	}
	logf("workspace grow: %s filesystem %d MiB < device %d MiB, resizing online", dev, fsBytes>>20, devBytes>>20)
	ctx, cancel := context.WithTimeout(context.Background(), wsGrowTimeout)
	defer cancel()
	out, err := wsResizeCmd(ctx, dev)
	if err != nil {
		logf("workspace grow: resize2fs %s failed, continuing at current size: %v: %s", dev, err, bytes.TrimSpace(out))
		return false
	}
	logf("workspace grow: resize2fs %s ok", dev)
	return true
}
