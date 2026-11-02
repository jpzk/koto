package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"koto/ociimage"
)

// A group with no image= boots the catalog default through the image path,
// with the shipped guest.img as vda, and its config.json is not rewritten
// (a 1.8.0 rollback must still see a no-image group).
func TestFcImagePlanForNoImageBootsDefault(t *testing.T) {
	fcHarness(t)
	t.Setenv(fcImageAgentEnv, "")
	dg := testCatalogImage(t, ociimage.FedoraName, "fed")
	st := fcImageStore()
	diff := ociimage.Sha([]byte("diff-fed"))
	fakeExt4(t, ociimage.LayerFile(st.Root, diff), ociimage.LayerLabel(diff), ociimage.LayerUUID(diff))
	fakeExt4(t, fcGuestPath(), ociimage.GuestLabel, "")
	calls := 0
	fcEnsureDefault = func(func(string, ...any)) (string, string, error) {
		calls++
		return ociimage.FedoraName, dg, nil
	}
	_ = os.MkdirAll(filepath.Dir(groupConfigPath("tg")), 0o755)
	// sys.img already made for this digest (kept, so no mke2fs needed here).
	_ = os.MkdirAll(filepath.Join(vol("tg"), ".cs"), 0o755)
	if err := os.WriteFile(filepath.Join(vol("tg"), "sys.img"), []byte("sys"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vol("tg"), "sys.digest"), []byte(dg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(groupConfigPath("tg"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := fcImagePlanFor("tg", false)
	if err != nil || p == nil {
		t.Fatalf("plan: %v %v", p, err)
	}
	if calls != 1 || p.Name != ociimage.FedoraName || p.Digest != dg || p.Tools != fcGuestPath() || len(p.Layers) != 1 {
		t.Fatalf("plan %+v (calls %d)", p, calls)
	}
	if b, _ := os.ReadFile(groupConfigPath("tg")); string(b) != `{}` {
		t.Fatalf("config.json rewritten: %s", b)
	}
	// A group pinned explicitly does not consult the default.
	if err := os.WriteFile(groupConfigPath("tg"), []byte(`{"image":"fedora","image_digest":"`+dg+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, err := fcImagePlanFor("tg", false); err != nil || p == nil || calls != 1 {
		t.Fatalf("pinned plan: %v %v calls %d", p, err, calls)
	}
}

// Offline and no rootfs.img to fall back to: refuse with the cause, never
// boot something else.
func TestFcImagePlanForNoImageOfflineRefused(t *testing.T) {
	fcHarness(t)
	_, err := fcImagePlanFor("tg", false)
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("want offline refusal, got %v", err)
	}
	// With rootfs.img on disk (upgraded 1.8.0 host) the same boot falls back.
	fcLegacyRootfs(t)
	if p, err := fcImagePlanFor("tg", false); p != nil || err != nil {
		t.Fatalf("legacy fallback: %v %v", p, err)
	}
}

// image=koto-default is a one-release opt-out that needs rootfs.img.
func TestFcImagePlanForKotoDefaultNeedsRootfs(t *testing.T) {
	fcHarness(t)
	_ = os.MkdirAll(filepath.Dir(groupConfigPath("tg")), 0o755)
	if err := os.WriteFile(groupConfigPath("tg"), []byte(`{"image":"`+ociimage.DefaultName+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fcImagePlanFor("tg", false); err == nil || !strings.Contains(err.Error(), "rootfs.img") {
		t.Fatalf("want rootfs.img refusal, got %v", err)
	}
	fcLegacyRootfs(t)
	if p, err := fcImagePlanFor("tg", false); p != nil || err != nil {
		t.Fatalf("opt-out: %v %v", p, err)
	}
}

// guest.img wins; rootfs.img is only the fallback; neither is an error.
func TestFcGuestSource(t *testing.T) {
	fcHarness(t)
	t.Setenv(fcImageAgentEnv, "")
	if _, _, err := fcGuestSource(); err == nil || !strings.Contains(err.Error(), "guest.img missing") {
		t.Fatalf("no assets: %v", err)
	}
	fcLegacyRootfs(t)
	src, kind, err := fcGuestSource()
	if _, ok := src.(ociimage.RootfsTools); err != nil || !ok || !strings.Contains(kind, "legacy") {
		t.Fatalf("rootfs only: %T %q %v", src, kind, err)
	}
	if fcLegacyToolsKey() == "" {
		t.Fatal("legacy source: gc must keep its tools key")
	}
	fakeExt4(t, fcGuestPath(), ociimage.GuestLabel, "")
	src, kind, err = fcGuestSource()
	if g, ok := src.(ociimage.ShippedGuest); err != nil || !ok || g.Path != fcGuestPath() || kind != "guest.img" {
		t.Fatalf("guest.img: %T %q %v", src, kind, err)
	}
	if k := fcLegacyToolsKey(); k != "" {
		t.Fatalf("shipped guest.img: no tools key to keep, got %q", k)
	}
}
