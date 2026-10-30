package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"koto/ociimage"
)

func testDiffID(i int) string {
	return fmt.Sprintf("sha256:%064x", 0xabc000+i)
}

func testPlan(n int) *fcImagePlan {
	p := &fcImagePlan{Name: "debian", Digest: "sha256:" + strings.Repeat("d", 64),
		Created: time.Date(2025, 8, 1, 2, 3, 4, 0, time.UTC), Tools: "/h/tools.img", Sys: "/h/sys.img"}
	for i := 0; i < n; i++ {
		p.DiffIDs = append(p.DiffIDs, testDiffID(i))
		p.Layers = append(p.Layers, fmt.Sprintf("/h/layers/%d.ext4", i))
	}
	return p
}

func noRL() map[string]any { return map[string]any{} }

// The image drive list is derived from ociimage.FixedDevices: tools root,
// workspace, sys, then layers; with vsock + reserve the max-layer VM sits
// exactly at the firecracker device cap, one more layer is refused.
func TestFcImageDrivesFromFixedDevices(t *testing.T) {
	max := ociimage.LayerSlots()
	if max != 12 {
		t.Fatalf("layer slots = %d, want 12 (17 MMIO - %v)", max, ociimage.FixedDevices)
	}
	d, err := fcImageDrives(testPlan(max), "/a/workspace.img", noRL)
	if err != nil {
		t.Fatal(err)
	}
	nonDrive := 0
	for _, f := range ociimage.FixedDevices {
		if f == "vsock" || f == "reserve" {
			nonDrive++
		}
	}
	if len(d)+nonDrive != ociimage.FCMaxMMIODevices {
		t.Fatalf("drives %d + non-drive devices %d != cap %d", len(d), nonDrive, ociimage.FCMaxMMIODevices)
	}
	want := []string{"tools", "workspace", "sys"}
	for i := 0; i < max; i++ {
		want = append(want, fmt.Sprintf("layer%02d", i))
	}
	for i, w := range want {
		if d[i]["drive_id"] != w {
			t.Fatalf("drive %d = %v, want %s", i, d[i]["drive_id"], w)
		}
		root := d[i]["is_root_device"].(bool)
		ro := d[i]["is_read_only"].(bool)
		if root != (w == "tools") {
			t.Fatalf("drive %s root=%v", w, root)
		}
		if ro != (w != "workspace" && w != "sys") {
			t.Fatalf("drive %s read_only=%v", w, ro)
		}
	}
	if d[1]["path_on_host"] != "/a/workspace.img" || d[0]["path_on_host"] != fcJailTools || d[2]["path_on_host"] != fcJailSys || d[3]["path_on_host"] != "/a/L00.ext4" {
		t.Fatalf("paths: %v %v %v %v", d[0]["path_on_host"], d[1]["path_on_host"], d[2]["path_on_host"], d[3]["path_on_host"])
	}
	_, err = fcImageDrives(testPlan(max+1), "/a/workspace.img", noRL)
	if err == nil || !strings.Contains(err.Error(), "at most 12") {
		t.Fatalf("13 layers: want readable slot refusal, got %v", err)
	}
	if _, err := fcImageDrives(testPlan(0), "/a/workspace.img", noRL); err == nil {
		t.Fatal("0 layers accepted")
	}
	// An unwired fixed device must fail, not be silently skipped.
	old := ociimage.FixedDevices
	ociimage.FixedDevices = append(append([]string{}, old...), "balloon")
	defer func() { ociimage.FixedDevices = old }()
	if _, err := fcImageDrives(testPlan(1), "/a/workspace.img", noRL); err == nil || !strings.Contains(err.Error(), "balloon") {
		t.Fatalf("unwired fixed device: %v", err)
	}
}

// The cmdline is the fcguest/image.go contract (parseImageArgs).
func TestFcImageBootArgs(t *testing.T) {
	p := testPlan(3)
	s := fcImageBootArgs(p)
	if !strings.HasPrefix(s, "console=ttyS0 reboot=k panic=1 pci=off quiet init=/bin/fc-agent ") {
		t.Fatalf("prefix: %s", s)
	}
	kv := map[string]string{}
	for _, f := range strings.Fields(s) {
		if k, v, ok := strings.Cut(f, "="); ok {
			if _, dup := kv[k]; dup {
				t.Fatalf("duplicate key %s", k)
			}
			kv[k] = v
		}
	}
	ls := strings.Split(kv["koto.layers"], ",")
	if len(ls) != 3 {
		t.Fatalf("layers: %v", ls)
	}
	hex32 := regexp.MustCompile(`^[a-f0-9]{32}$`)
	for i, l := range ls {
		if !hex32.MatchString(l) || l != strings.TrimPrefix(testDiffID(i), "sha256:")[:32] {
			t.Fatalf("layer %d = %q (bottom->top order, hex32)", i, l)
		}
	}
	if kv["koto.image"] != "debian" || kv["koto.digest"] != p.Digest || kv["koto.created"] != "2025-08-01T02:03:04Z" {
		t.Fatalf("keys: %v", kv)
	}
	p.Created = time.Time{}
	if strings.Contains(fcImageBootArgs(p), "koto.created") {
		t.Fatal("zero created must be omitted")
	}
}

func TestFcImageDedupKeepsTopmost(t *testing.T) {
	a, b, c := ociimage.Layer{DiffID: "A"}, ociimage.Layer{DiffID: "B"}, ociimage.Layer{DiffID: "C"}
	got := fcImageDedup([]ociimage.Layer{a, b, a, c})
	if len(got) != 3 || got[0].DiffID != "B" || got[1].DiffID != "A" || got[2].DiffID != "C" {
		t.Fatalf("dedup: %v", got)
	}
}

func TestFcVMConfigForImage(t *testing.T) {
	b, err := fcVMConfigFor("g", "/a/vmlinux", "/a/rootfs.img", "/a/workspace.img", "/vsock/v", 2, 1024, 1, 1, false, testPlan(2))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Boot   map[string]any   `json:"boot-source"`
		Drives []map[string]any `json:"drives"`
		Vsock  map[string]any   `json:"vsock"`
		CPU    any              `json:"cpu-config"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Drives) != 5 || cfg.Vsock["uds_path"] != "/vsock/v" || cfg.CPU == nil || cfg.Boot["kernel_image_path"] != "/a/vmlinux" {
		t.Fatalf("cfg: %s", b)
	}
	if !strings.Contains(cfg.Boot["boot_args"].(string), "koto.layers=") || strings.Contains(string(b), "rootfs.img") {
		t.Fatalf("image config still references the rootfs or lacks koto.layers: %s", b)
	}
	if _, err := fcVMConfigFor("g", "/k", "/r", "/w", "/v", 2, 1024, 1, 1, false, testPlan(13)); err == nil {
		t.Fatal("13-layer plan produced a config")
	}
}

func TestFcStageJailForImage(t *testing.T) {
	fcHarness(t)
	def, err := fcStageJail("tg", []byte("{}"), 4242)
	if err != nil {
		t.Fatal(err)
	}
	nilp, err := fcStageJailFor("tg", []byte("{}"), 4242, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(def) != fmt.Sprint(nilp) {
		t.Fatalf("nil plan jail differs from default")
	}
	_ = os.RemoveAll(fcJailDir("tg"))
	p := testPlan(2)
	sp, err := fcStageJailFor("tg", []byte("{}"), 4242, p)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fcBind{}
	for _, b := range sp.Binds {
		got[b.Dst] = b
	}
	if _, ok := got["/a/rootfs.img"]; ok {
		t.Fatal("image jail still binds the rootfs")
	}
	if b := got[fcJailTools]; b.Src != p.Tools || !b.RO {
		t.Fatalf("tools bind %+v", b)
	}
	if b := got[fcJailSys]; b.Src != p.Sys || b.RO {
		t.Fatalf("sys bind %+v", b)
	}
	for i := range p.Layers {
		if b := got[fcJailLayer(i)]; b.Src != p.Layers[i] || !b.RO {
			t.Fatalf("layer %d bind %+v", i, b)
		}
	}
	if len(sp.Binds) != len(def.Binds)-1+2+len(p.Layers) {
		t.Fatalf("bind count %d", len(sp.Binds))
	}
	for _, b := range sp.Binds {
		if _, err := os.Stat(filepath.Join(sp.Chroot, b.Dst)); err != nil {
			t.Fatalf("bind target %s not staged: %v", b.Dst, err)
		}
	}
}

// fcImagePlanFor: no image / built-in = default boot; anything that cannot
// be resolved from the catalog refuses the boot instead of falling back.
func TestFcImagePlanForRefusals(t *testing.T) {
	fcHarness(t)
	set := func(cfg map[string]any) {
		_ = os.MkdirAll(filepath.Dir(groupConfigPath("tg")), 0o755)
		b, _ := json.Marshal(cfg)
		if err := os.WriteFile(groupConfigPath("tg"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, cfg := range []map[string]any{{}, {"image": ""}, {"image": ociimage.DefaultName}} {
		set(cfg)
		if p, err := fcImagePlanFor("tg"); p != nil || err != nil {
			t.Fatalf("%v: want default boot, got %v %v", cfg, p, err)
		}
	}
	dg := "sha256:" + strings.Repeat("e", 64)
	for cfg, want := range map[string]string{
		`{"image":"debian:trixie","image_digest":"` + dg + `"}`: "not a catalog name",
		`{"image":"debian"}`:                             "no valid image_digest",
		`{"image":"debian","image_digest":"latest"}`:     "no valid image_digest",
		`{"image":"debian","image_digest":"` + dg + `"}`: "not in the catalog",
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(cfg), &m)
		set(m)
		p, err := fcImagePlanFor("tg")
		if p != nil || err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: want error %q, got %v %v", cfg, want, p, err)
		}
	}
}

// The default-rootfs boot parks an image group's sys.img under its digest, so
// returning to the same image later is a real transition (greptile: A ->
// koto-default -> A kept the old sys.img despite image_sys=fresh).
func TestFcImagePlanForDefaultArchivesSys(t *testing.T) {
	fcHarness(t)
	dg := "sha256:" + strings.Repeat("a", 64)
	_ = os.MkdirAll(vol("tg"), 0o755)
	if err := os.WriteFile(filepath.Join(vol("tg"), "sys.img"), []byte("A-state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vol("tg"), "sys.digest"), []byte(dg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := fcImagePlanFor("tg"); p != nil || err != nil {
		t.Fatalf("default boot: %v %v", p, err)
	}
	if _, err := os.Stat(filepath.Join(vol("tg"), "sys.img")); err == nil {
		t.Fatal("sys.img still in place after a default boot")
	}
	if ociimage.SysDigest(vol("tg")) != "" {
		t.Fatal("sys.digest kept")
	}
	if b, err := os.ReadFile(ociimage.SysArchive(vol("tg"), dg)); err != nil || string(b) != "A-state" {
		t.Fatalf("archive: %q %v", b, err)
	}
}

// qodo round 2: root=yes -> root=no must not keep a sys overlay (or a
// parked archive a rollback could restore) that a root worker wrote.
func TestFcImageSysPostureDowngradeDiscards(t *testing.T) {
	fcHarness(t)
	v := vol("tg")
	cs := filepath.Join(v, ".cs")
	_ = os.MkdirAll(cs, 0o755)
	setRoot := func(r string) {
		if err := os.WriteFile(filepath.Join(cs, "config.json"), []byte(`{"root":"`+r+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dgA := "sha256:" + strings.Repeat("a", 64)
	dgB := "sha256:" + strings.Repeat("b", 64)
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(v, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var logs []string
	logf := func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }

	// root=no with no history: nothing touched
	setRoot("no")
	write("sys.img", "clean")
	write("sys.digest", dgA+"\n")
	if err := fcImageSysPosture("tg", logf); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(v, "sys.img")); string(b) != "clean" {
		t.Fatal("clean root=no sys.img touched")
	}
	// root=yes era: marked, disks kept while root=yes
	setRoot("yes")
	if err := fcImageSysPosture("tg", logf); err != nil {
		t.Fatal(err)
	}
	write("sys.img", "root-written")
	write(filepath.Base(ociimage.SysArchive(v, dgB)), "root-archive")
	if err := fcImageSysPosture("tg", logf); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(v, "sys.img")); string(b) != "root-written" {
		t.Fatal("root=yes boot dropped its own sys.img")
	}
	// downgrade: every root-era disk goes, and only once
	setRoot("no")
	if err := fcImageSysPosture("tg", logf); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"sys.img", "sys.digest", filepath.Base(ociimage.SysArchive(v, dgB)), "sys.root"} {
		if _, err := os.Stat(filepath.Join(v, p)); err == nil {
			t.Errorf("%s kept after root=yes -> root=no", p)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "discarded 2 system disk") {
		t.Fatalf("logs: %q", logs)
	}
	write("sys.img", "fresh")
	write("sys.digest", dgA+"\n")
	if err := fcImageSysPosture("tg", logf); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(v, "sys.img")); string(b) != "fresh" {
		t.Fatal("second root=no boot discarded again")
	}
}
