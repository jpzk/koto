package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	"koto/ociimage"
)

// testCatalogImage adds name -> a fake image (digest derived from seed) to
// the harness's image catalog and returns its digest.
func testCatalogImage(t *testing.T, name, seed string) string {
	t.Helper()
	img := &ociimage.Image{Digest: ociimage.Sha([]byte("idx-" + seed)), Ref: "example.test/" + seed,
		Arch: "amd64", Created: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		Layers: []ociimage.Layer{{Digest: ociimage.Sha([]byte("blob-" + seed)), DiffID: ociimage.Sha([]byte("diff-" + seed)), Size: 10}}}
	if err := fcImageStore().Update(func(c *ociimage.Catalog) error {
		_, err := c.Put(name, img, ociimage.LibcGlibc, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return img.Digest
}

func ctlConfigSet(t *testing.T, line string) map[string]any {
	t.Helper()
	b, _ := json.Marshal(ctlDispatch(ctlMainGroup, []byte(line)))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// config_set image= accepts ONLY catalog names: raw refs (tag, digest,
// registry path), unknown names, invalid names and non-strings are refused
// with a readable error and the config is left untouched; a catalog name is
// pinned to its digest at set time.
func TestConfigSetImageRejectsNonCatalog(t *testing.T) {
	fcHarness(t)
	dg := testCatalogImage(t, "debian", "a")
	for val, want := range map[string]string{
		`"debian:trixie"`:                   "registry reference",
		`"docker.io/library/debian"`:        "registry reference",
		`"debian@` + dg + `"`:               "registry reference",
		`"alpine"`:                          "not in the image catalog",
		`"Debian"`:                          "not a valid catalog name",
		`"` + strings.Repeat("x", 70) + `"`: "not a valid catalog name",
		`"x\u001b]52;c;AAAA\u0007"`:         "not a valid catalog name",
		`42`:                                "must be a string",
	} {
		r := ctlConfigSet(t, `{"cmd":"config_set","group":"tg","image":`+val+`}`)
		if r["ok"] == true || !strings.Contains(fmt.Sprint(r["error"]), want) {
			t.Fatalf("image=%s: want refusal %q, got %v", val, want, r)
		}
		if c := loadGroupConfig("tg"); c.str("image") != "" || c.str("image_digest") != "" {
			t.Fatalf("image=%s: config written despite refusal: %v", val, c)
		}
	}
	// The gRPC/CLI path converges on configCmd: same refusal.
	s := "ghcr.io/x/y:1"
	if r := configCmd(fromPBConfigReqImage("tg", &s)); r.OK || !strings.Contains(r.Error, "registry reference") {
		t.Fatalf("configCmd raw ref: %+v", r)
	}
	// A catalog name is accepted (main may pick a peer's base image; it is not
	// a posture key) and pinned to the digest it resolves to NOW.
	r := ctlConfigSet(t, `{"cmd":"config_set","group":"tg","image":"debian"}`)
	if r["ok"] != true {
		t.Fatalf("catalog name refused: %v", r)
	}
	c := loadGroupConfig("tg")
	if c.str("image") != "debian" || c.str("image_digest") != dg || c.str("image_sys") != "fresh" ||
		c.str("image_prev") != ociimage.DefaultName {
		t.Fatalf("pin not recorded: %v", c)
	}
	// Moving the catalog name afterwards does not move the group.
	testCatalogImage(t, "debian", "b")
	if c := loadGroupConfig("tg"); c.str("image_digest") != dg {
		t.Fatalf("catalog update moved the group: %v", c)
	}
	// Setting the (moved) name again pins the new digest and keeps the old
	// one for rollback.
	ctlConfigSet(t, `{"cmd":"config_set","group":"tg","image":"debian"}`)
	c = loadGroupConfig("tg")
	if c.str("image_digest") == dg || c.str("image_prev") != dg || c.str("image_prev_name") != "debian" {
		t.Fatalf("re-pin: %v", c)
	}
	// Clearing returns to the default rootfs, remembering the pin.
	cur := c.str("image_digest")
	ctlConfigSet(t, `{"cmd":"config_set","group":"tg","image":""}`)
	c = loadGroupConfig("tg")
	if c.str("image") != "" || c.str("image_digest") != "" || c.str("image_prev") != cur {
		t.Fatalf("clear: %v", c)
	}
	if p, err := fcImagePlanFor("tg"); p != nil || err != nil {
		t.Fatalf("cleared group must boot the default rootfs: %v %v", p, err)
	}
	// The effective config always names the image.
	if eff := configCmd(configReq{Group: "tg"}).Config; eff["image"] != ociimage.DefaultName {
		t.Fatalf("effective image = %v", eff["image"])
	}
}

func fromPBConfigReqImage(g string, s *string) configReq {
	b, _ := json.Marshal(*s)
	return configReq{Group: g, Image: b}
}

// Re-setting the digest a group already runs is a no-op for sys/rollback.
func TestSetImageConfigSameDigestKeepsState(t *testing.T) {
	dg := "sha256:" + strings.Repeat("a", 64)
	cfg := map[string]any{"image": "debian", "image_digest": dg, "image_sys": "restore", "image_prev": "x"}
	setImageConfig(cfg, imagePin{Name: "deb2", Digest: dg})
	if cfg["image"] != "deb2" || cfg["image_sys"] != "restore" || cfg["image_prev"] != "x" {
		t.Fatalf("%v", cfg)
	}
}

func TestCtlGroupAndFlags(t *testing.T) {
	for _, args := range [][]string{{"g1", "--image", "debian"}, {"-image", "debian", "g1"}} {
		fs := newSpawnFlagsForTest()
		img := fs.String("image", "", "")
		g, err := ctlGroupAndFlags(fs, args)
		if err != nil || g != "g1" || *img != "debian" {
			t.Fatalf("%v: %q %q %v", args, g, *img, err)
		}
	}
	fs := newSpawnFlagsForTest()
	if _, err := ctlGroupAndFlags(fs, []string{"g1", "g2"}); err == nil {
		t.Fatal("two groups accepted")
	}
}

func newSpawnFlagsForTest() *flag.FlagSet { return flag.NewFlagSet("t", flag.ContinueOnError) }
