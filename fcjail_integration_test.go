package main

// fcjail_integration_test.go — boots the REAL firecracker binary through the
// production jail path (fcStageJail + fcJailCommand + the `fcjail` shim) and
// asserts the guest kernel reaches KVM. Exercises the actual staging, bind
// mounts, chroot, uid drop, and exec — not a reimplementation.
//
// Gated behind CLAWSON_FC_JAIL_IT=1 because it needs /dev/kvm, the fetched
// assets (fcassets/{firecracker,vmlinux,rootfs.img}), and the ability to
// create nested user namespaces — i.e. it runs inside clawson-host, not in
// unit-test CI. Run:
//
//   go test -run TestFcJailBootsReal -v   (with CLAWSON_FC_JAIL_IT=1 + assets)

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFcJailBootsReal(t *testing.T) {
	if os.Getenv("CLAWSON_FC_JAIL_IT") != "1" {
		t.Skip("integration test; set CLAWSON_FC_JAIL_IT=1 (needs /dev/kvm + assets)")
	}
	// Assets live in the real project tree; point HERE there so fcBinPath/
	// fcKernelPath/fcRootfsPath resolve, but keep run/ + groups/ in a tempdir.
	proj := os.Getenv("CLAWSON_PROJ")
	if proj == "" {
		proj = "/home/<user>/clawson"
	}
	HERE = proj
	tmp := t.TempDir()
	// The shim must re-exec the real daemon binary (its main() dispatches
	// `fcjail`); os.Executable() here is the test binary, which would rerun the
	// suite instead. Build the real binary once and point the jailer at it.
	self := filepath.Join(tmp, "clawson")
	run(t, "go", "build", "-o", self, ".")
	fcJailSelfExe = self
	ROOT = filepath.Join(tmp, "groups")
	SOCK_DIR = filepath.Join(tmp, "run")
	PORT_BASE = 8787
	if err := os.MkdirAll(fcRunDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fcBinPath()); err != nil {
		t.Skipf("assets missing: %v", err)
	}

	const g = "jt"
	if err := os.MkdirAll(fcSockDir(g), 0o755); err != nil {
		t.Fatal(err)
	}
	// A small real ext4 workspace image (the guest mounts it; boot doesn't
	// depend on its contents, only that FC can open it read-write as the
	// dropped uid).
	ws := fcWorkspaceImg(g)
	if err := os.MkdirAll(filepath.Dir(ws), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "truncate", "-s", "64M", ws)
	run(t, "mkfs.ext4", "-qF", ws)

	uid := fcJailUID(8787)
	if err := fcJailFixupPerms(g, uid); err != nil {
		t.Fatalf("fixup perms: %v", err)
	}
	// Build the same chroot-relative config fcSpawn writes when jailed.
	cfg := []byte(`{"boot-source":{"kernel_image_path":"/a/vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 pci=off"},` +
		`"drives":[{"drive_id":"rootfs","path_on_host":"/a/rootfs.img","is_root_device":true,"is_read_only":true},` +
		`{"drive_id":"workspace","path_on_host":"/a/workspace.img","is_root_device":false,"is_read_only":false}],` +
		`"machine-config":{"vcpu_count":1,"smt":false,"mem_size_mib":512},` +
		`"vsock":{"guest_cid":3,"uds_path":"/vsock/v"}}`)

	spec, err := fcStageJail(g, cfg, uid)
	if err != nil {
		t.Fatalf("stage jail: %v", err)
	}
	cmd, err := fcJailCommand(spec)
	if err != nil {
		t.Fatalf("jail command: %v", err)
	}
	console := filepath.Join(tmp, "console.log")
	cf, err := os.Create(console)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = cf, cf
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(4 * time.Second)
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	cf.Close()

	out, _ := os.ReadFile(console)
	got := string(out)
	t.Logf("console:\n%s", got)
	for _, want := range []string{"Running Firecracker", "Hypervisor detected: KVM"} {
		if !strings.Contains(got, want) {
			t.Fatalf("console missing %q — jail broke the boot", want)
		}
	}
	// Prove the drop actually happened: the shim logs its uid before exec.
	if !strings.Contains(got, "Successfully started microvm") {
		t.Errorf("microvm did not fully configure (see console)")
	}
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
