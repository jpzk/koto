package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	hA = "0123456789abcdef0123456789abcdef"
	hB = "fedcba9876543210fedcba9876543210"
	hC = "aaaaaaaaaaaaaaaabbbbbbbbbbbbbbbb"
	dg = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func TestParseImageArgsDefaultBoot(t *testing.T) {
	a, err := parseImageArgs("console=ttyS0 reboot=k panic=1 pci=off quiet init=/usr/local/bin/fc-agent")
	if a != nil || err != nil {
		t.Fatalf("default cmdline must not enter image mode: %+v %v", a, err)
	}
}

func TestParseImageArgs(t *testing.T) {
	cl := "console=ttyS0 init=/bin/fc-agent koto.layers=" + hA + "," + hB +
		" koto.image=debian koto.digest=" + dg + " koto.created=2025-08-12T00:00:00Z"
	a, err := parseImageArgs(cl)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Layers) != 2 || a.Layers[0] != hA || a.Layers[1] != hB || a.Name != "debian" ||
		a.Digest != dg || a.Created != "2025-08-12T00:00:00Z" {
		t.Fatalf("parsed %+v", a)
	}
}

func TestParseImageArgsRejects(t *testing.T) {
	ok := " koto.image=debian koto.digest=" + dg
	for name, cl := range map[string]string{
		"empty":       "koto.layers=" + ok,
		"short hex":   "koto.layers=0123" + ok,
		"upper hex":   "koto.layers=" + strings.ToUpper(hA) + ok,
		"dup layer":   "koto.layers=" + hA + "," + hA + ok,
		"dup key":     "koto.layers=" + hA + " koto.layers=" + hB + ok,
		"raw ref":     "koto.layers=" + hA + " koto.image=docker.io/library/debian:trixie koto.digest=" + dg,
		"no digest":   "koto.layers=" + hA + " koto.image=debian",
		"bad created": "koto.layers=" + hA + ok + " koto.created=a;b",
		"too many":    "koto.layers=" + manyLayers(17) + ok,
	} {
		if a, err := parseImageArgs(cl); err == nil {
			t.Errorf("%s: accepted %+v", name, a)
		}
	}
}

func manyLayers(n int) string {
	var s []string
	for i := 0; i < n; i++ {
		s = append(s, strings.Repeat("0", 30)+string("0123456789abcdef"[i/16])+string("0123456789abcdef"[i%16]))
	}
	return strings.Join(s, ",")
}

func sb(dev, h string) superblock { return superblock{dev, layerLabel(h), layerUUID(h)} }

func TestMatchLayersBySuperblockNotOrder(t *testing.T) {
	// drives deliberately in a different order than the cmdline
	drives := []superblock{
		{"/dev/vda", "kototools", "x"}, {"/dev/vdc", "kotosys", "y"},
		sb("/dev/vdd", hC), sb("/dev/vde", hA), sb("/dev/vdf", hB),
	}
	got, err := matchLayers([]string{hA, hB, hC}, drives)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/dev/vde", "/dev/vdf", "/dev/vdd"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v want %v", got, want)
	}
	if o := lowerdirOpt([]string{"/mnt/l/00", "/mnt/l/01", "/mnt/l/02"}); o != "lowerdir=/mnt/l/02:/mnt/l/01:/mnt/l/00" {
		t.Fatalf("lowerdir must list top first: %s", o)
	}
}

func TestMatchLayersRefuses(t *testing.T) {
	sub := sb("/dev/vdd", hA)
	sub.uuid = layerUUID(hB) // right label, wrong uuid: a substituted drive
	cases := map[string][]superblock{
		"missing":     {sb("/dev/vdd", hB)},
		"wrong uuid":  {sub},
		"on 2 drives": {sb("/dev/vdd", hA), sb("/dev/vde", hA)},
	}
	for name, drives := range cases {
		if _, err := matchLayers([]string{hA}, drives); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			t.Logf("%s: %v", name, err)
		}
	}
}

func TestLayerNamingMatchesHost(t *testing.T) {
	// must stay identical to daemon/ociimage LayerLabel / LayerUUID
	if l := layerLabel(hA); l != "L0123456789abcde" {
		t.Fatal(l)
	}
	if u := layerUUID(hA); u != "01234567-89ab-cdef-0123-456789abcdef" {
		t.Fatal(u)
	}
}

func TestParseSuperblock(t *testing.T) {
	b := make([]byte, 1024)
	if _, err := parseSuperblock("x", b); err == nil {
		t.Fatal("no magic accepted")
	}
	b[0x38], b[0x39] = 0x53, 0xEF
	for i := 0; i < 16; i++ {
		b[0x68+i] = byte(0x01*i + 0x10*i)
	}
	copy(b[0x78:], "kotosys")
	s, err := parseSuperblock("x", b)
	if err != nil || s.label != "kotosys" || s.uuid != "00112233-4455-6677-8899-aabbccddeeff" {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestInjectImage(t *testing.T) {
	root := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "etc"), 0o755))
	must(os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0:root:/root:/bin/bash"), 0o644))
	must(os.WriteFile(filepath.Join(root, "etc/group"), []byte("root:x:0:\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "etc/resolv.conf"), []byte("nameserver 9.9.9.9\n"), 0o644))
	a := &imageArgs{Layers: []string{hA}, Name: "debian", Digest: dg, Created: "2025-08-12T00:00:00Z"}
	for i := 0; i < 2; i++ { // idempotent across boots
		must(injectImage(root, a))
	}
	pw, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if strings.Count(string(pw), ":1000:1000:") != 1 || !strings.Contains(string(pw), "\nnode:x:1000:1000:koto worker:/workspace:") {
		t.Fatalf("passwd:\n%s", pw)
	}
	gr, _ := os.ReadFile(filepath.Join(root, "etc/group"))
	if strings.Count(string(gr), "node:x:1000:") != 1 {
		t.Fatalf("group:\n%s", gr)
	}
	if l, _ := os.Readlink(filepath.Join(root, "etc/resolv.conf")); l != "/run/resolv.conf" {
		t.Fatalf("resolv.conf -> %q", l)
	}
	if l, _ := os.Readlink(filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")); l != "/opt/koto/etc/ssl/ca-bundle.crt" {
		t.Fatalf("ca -> %q", l)
	}
	if l, _ := os.Readlink(filepath.Join(root, "sidecar")); l != "/opt/koto/sidecar" {
		t.Fatalf("sidecar -> %q", l)
	}
	img, _ := os.ReadFile(filepath.Join(root, "etc/koto/image"))
	if string(img) != "name=debian\ndigest="+dg+"\ncreated=2025-08-12T00:00:00Z\n" {
		t.Fatalf("/etc/koto/image:\n%s", img)
	}
	if fi, _ := os.Stat(filepath.Join(root, "etc/koto/image")); fi.Mode().Perm() != 0o444 {
		t.Fatalf("/etc/koto/image mode %v", fi.Mode())
	}
	hosts, _ := os.ReadFile(filepath.Join(root, "etc/hosts"))
	if strings.Count(string(hosts), "koto-vm") != 1 {
		t.Fatalf("hosts:\n%s", hosts)
	}
}

func TestInjectImageKeepsExistingWorkerAndCA(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "etc/ssl/certs"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root:x:0:0::/root:/bin/sh\nnode:x:1000:1000::/home/node:/bin/bash\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "etc/group"), []byte("root:x:0:\nnode:x:1000:\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "etc/ssl/certs/ca-certificates.crt"), []byte("PEM"), 0o644)
	if err := injectImage(root, &imageArgs{Name: "node", Digest: dg}); err != nil {
		t.Fatal(err)
	}
	pw, _ := os.ReadFile(filepath.Join(root, "etc/passwd"))
	if strings.Count(string(pw), ":1000:") != 1 {
		t.Fatalf("uid 1000 duplicated:\n%s", pw)
	}
	if fi, _ := os.Lstat(filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("image CA bundle replaced")
	}
}

func TestWorkerPATHDefaultUnchanged(t *testing.T) {
	if imageMode == nil && workerPATH != "/sidecar:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Fatalf("default worker PATH changed: %s", workerPATH)
	}
}

// provision.sh is worker-writable, so it must run with the group's posture:
// root only for root=yes, uid/gid 1000 with no supplementary groups otherwise.
func TestProvisionCmdHonoursRootPosture(t *testing.T) {
	w := provisionCmd(false)
	c := w.SysProcAttr
	if c == nil || c.Credential == nil {
		t.Fatal("root=no: provision.sh would run as root")
	}
	if c.Credential.Uid != workerUID || c.Credential.Gid != workerGID || len(c.Credential.Groups) != 0 || c.Credential.NoSetGroups {
		t.Fatalf("root=no credential = %+v, want uid/gid 1000, no groups", c.Credential)
	}
	if w.Dir != wsDir || !hasEnv(w.Env, "HOME="+wsDir) || !hasEnv(w.Env, "USER=node") {
		t.Fatalf("root=no dir/env = %q %q", w.Dir, w.Env)
	}
	if got := provisionWho(w); got != "uid 1000" {
		t.Fatalf("who = %q", got)
	}
	r := provisionCmd(true)
	if r.SysProcAttr != nil && r.SysProcAttr.Credential != nil {
		t.Fatalf("root=yes: unexpected credential %+v", r.SysProcAttr.Credential)
	}
	if r.Dir != "/root" || !hasEnv(r.Env, "HOME=/root") || provisionWho(r) != "root" {
		t.Fatalf("root=yes dir/env = %q %q", r.Dir, r.Env)
	}
	for _, cmd := range []*exec.Cmd{w, r} {
		if len(cmd.Args) != 2 || cmd.Args[1] != provisionScript {
			t.Fatalf("args = %q", cmd.Args)
		}
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}

func TestRevokeImageSudoOnlyKotoGrant(t *testing.T) {
	d := t.TempDir()
	ours := filepath.Join(d, "node")
	if err := os.WriteFile(ours, []byte(imageSudoGrant), 0o440); err != nil {
		t.Fatal(err)
	}
	revokeImageSudo(ours)
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Fatalf("koto grant not removed: %v", err)
	}
	theirs := filepath.Join(d, "other")
	if err := os.WriteFile(theirs, []byte("node ALL=(ALL) ALL\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	revokeImageSudo(theirs)
	if _, err := os.Stat(theirs); err != nil {
		t.Fatalf("foreign sudoers file removed: %v", err)
	}
	revokeImageSudo(filepath.Join(d, "missing")) // no panic, no error
}
