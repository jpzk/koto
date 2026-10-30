package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"koto/ociimage"
)

type imageTestEnv struct {
	imageEnv
	restarted []string
	up        map[string]bool
	log       strings.Builder
}

func newImageTestEnv(t *testing.T) *imageTestEnv {
	t.Helper()
	e := &imageTestEnv{up: map[string]bool{}}
	e.imageEnv = imageEnv{
		store:   fcImageStore(),
		client:  &ociimage.Client{},
		groups:  func() []string { return []string{"main", "peer", "tg"} },
		running: func(g string) bool { return e.up[g] },
		restart: func(g string) error { e.restarted = append(e.restarted, g); return nil },
		tools:   func() string { return "" },
		now:     time.Now,
	}
	return e
}

func (e *imageTestEnv) run(op string, all bool, args ...string) error {
	return imageRun(e.imageEnv, op, args, all, func(f string, a ...any) { fmt.Fprintf(&e.log, f+"\n", a...) })
}

// apply pins groups to a catalog digest (restarting only running ones);
// --all moves every group set to the name after an update; rm is refused
// while a group uses the image; rollback swaps current/previous and asks
// for the archived sys.img (restore), including back to koto-default.
func TestImageApplyRollbackRm(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	dg1 := testCatalogImage(t, "debian", "a")

	if err := e.run("apply", false, "debian", "tg"); err != nil {
		t.Fatal(err)
	}
	c := loadGroupConfig("tg")
	if c.str(cfgImageDigest) != dg1 || c.str(cfgImagePrev) != ociimage.DefaultName || c.str(cfgImageSys) != "fresh" {
		t.Fatalf("apply: %v", c)
	}
	if len(e.restarted) != 0 {
		t.Fatalf("stopped group restarted: %v", e.restarted)
	}

	// update moved the name; groups stay until apply --all.
	dg2 := testCatalogImage(t, "debian", "b")
	if loadGroupConfig("tg").str(cfgImageDigest) != dg1 {
		t.Fatal("catalog move moved the group")
	}
	e.up["tg"] = true
	if err := e.run("apply", true, "debian"); err != nil {
		t.Fatal(err)
	}
	c = loadGroupConfig("tg")
	if c.str(cfgImageDigest) != dg2 || c.str(cfgImagePrev) != dg1 || c.str(cfgImagePrevName) != "debian" || c.str(cfgImageSys) != "fresh" {
		t.Fatalf("apply --all: %v", c)
	}
	if strings.Join(e.restarted, ",") != "tg" {
		t.Fatalf("restarts: %v", e.restarted)
	}

	// in use: refused, catalog unchanged.
	err := e.run("rm", false, "debian")
	if !errors.Is(err, ociimage.ErrInUse) || !strings.Contains(err.Error(), "tg") {
		t.Fatalf("rm in use: %v", err)
	}
	if cat, _ := fcImageStore().Load(); cat.Names["debian"] != dg2 {
		t.Fatal("rm changed the catalog despite refusal")
	}

	// rollback: back to dg1 with its archived sys.img.
	if err := e.run("rollback", false, "tg"); err != nil {
		t.Fatal(err)
	}
	c = loadGroupConfig("tg")
	if c.str(cfgImageDigest) != dg1 || c.str(cfgImageSys) != "restore" || c.str(cfgImagePrev) != dg2 || c.str(cfgImage) != "debian" {
		t.Fatalf("rollback: %v", c)
	}
	if len(e.restarted) != 2 {
		t.Fatalf("rollback did not restart: %v", e.restarted)
	}

	// rollback to the default rootfs.
	if err := e.run("apply", false, "debian", "peer"); err != nil {
		t.Fatal(err)
	}
	if err := e.run("rollback", false, "peer"); err != nil {
		t.Fatal(err)
	}
	c = loadGroupConfig("peer")
	if c.str(cfgImage) != "" || c.str(cfgImageDigest) != "" || c.str(cfgImagePrev) != dg2 {
		t.Fatalf("rollback to default: %v", c)
	}
	if p, err := fcImagePlanFor("peer"); p != nil || err != nil {
		t.Fatalf("default plan: %v %v", p, err)
	}

	// readable refusals.
	for _, tc := range []struct {
		op   string
		all  bool
		args []string
		want string
	}{
		{"rollback", false, []string{"main"}, "no previous image"},
		{"rollback", false, []string{"nope"}, "no group"},
		{"apply", false, []string{"debian:trixie", "tg"}, "registry reference"},
		{"apply", false, []string{"debian"}, "usage"},
		{"apply", false, []string{"debian", "ghost"}, "no group"},
		{"update", false, []string{"debian"}, "no recorded source"},
		{"update", false, []string{"ghost"}, "no image named"},
		{"add", false, []string{"debian", "docker.io/library/debian:trixie"}, "already exists"},
		{"add", false, []string{"koto-default", "x/y:z"}, "built-in"},
		{"rm", false, []string{"koto-default"}, "built in"},
		{"frob", false, nil, "unknown image op"},
	} {
		if err := e.run(tc.op, tc.all, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s %v: want %q, got %v", tc.op, tc.args, tc.want, err)
		}
	}

	e.log.Reset()
	if err := e.run("ls", false); err != nil {
		t.Fatal(err)
	}
	ls := e.log.String()
	if !strings.Contains(ls, "koto-default") || !strings.Contains(ls, "debian  "+ociimage.Short(dg2)) ||
		!strings.Contains(ls, "used by peer,tg") || !strings.Contains(ls, "<unnamed>  "+ociimage.Short(dg1)+"  kept for tg") {
		t.Fatalf("ls:\n%s", ls)
	}

	// once nothing uses it, rm succeeds.
	for _, g := range []string{"tg", "peer"} {
		if _, err := updateGroupConfig(g, func(cfg map[string]any) {
			for _, k := range []string{cfgImage, cfgImageDigest, cfgImagePrev, cfgImagePrevName, cfgImageSys} {
				delete(cfg, k)
			}
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.run("rm", false, "debian"); err != nil {
		t.Fatal(err)
	}
}

func TestImageCliParse(t *testing.T) {
	r, err := imageCliParse([]string{"apply", "debian", "--all"})
	if err != nil || r.Op != "apply" || !r.All || len(r.Args) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := imageCliParse([]string{"rm", "x", "--all"}); err == nil {
		t.Fatal("--all accepted for rm")
	}
	if _, err := imageCliParse(nil); err == nil {
		t.Fatal("empty accepted")
	}
}

// F18: apply landing while a boot is between its running check and PID
// registration (fcSpawn holds groupOpMu across fcImagePlanFor) must not
// conclude "not running" and leave the VM on the old image. apply takes the
// group op lock, so it waits for the boot and then sees it up -> restart.
func TestImageApplyWaitsForInFlightSpawn(t *testing.T) {
	fcHarness(t)
	e := newImageTestEnv(t)
	testCatalogImage(t, "debian", "a")
	var mu sync.Mutex
	e.lock = func(string) func() { mu.Lock(); return mu.Unlock }

	mu.Lock() // a spawn of tg is in flight: lock held, PID not yet registered
	done := make(chan error, 1)
	go func() { done <- e.run("apply", false, "debian", "tg") }()
	select {
	case err := <-done:
		mu.Unlock()
		t.Fatalf("apply finished during an in-flight spawn (err=%v, restarts=%v)", err, e.restarted)
	case <-time.After(100 * time.Millisecond):
	}
	e.up["tg"] = true // spawn registers its PID, then releases the lock
	mu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.restarted, ",") != "tg" {
		t.Fatalf("booted-mid-apply group not restarted: %v", e.restarted)
	}
}
