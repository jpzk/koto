package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"koto/ociimage"
)

// testGzLayer is a one-file gzip'd layer: its Layer and blob bytes.
func testGzLayer(name, body string) (ociimage.Layer, []byte) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	var blob bytes.Buffer
	zw := gzip.NewWriter(&blob)
	zw.Write(raw.Bytes())
	zw.Close()
	return ociimage.Layer{Digest: ociimage.Sha(blob.Bytes()), DiffID: ociimage.Sha(raw.Bytes()),
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Size: int64(blob.Len())}, blob.Bytes()
}

// PR 94 round 1 live check (Esc 33 ms after the last blob landed): the
// cancel reached the daemon while the ubuntu layer was converting, the
// catalog gate held, but the 164 MiB layer ext4 stayed behind. An add whose
// rpc is cancelled during the first layer's build returns context.Canceled,
// never builds the second layer, writes no catalog entry and leaves no
// layer file.
func TestImageAddCancelDuringLayerBuildLeavesNoLayer(t *testing.T) {
	for _, tool := range []string{"mke2fs", "debugfs", "e2fsck"} {
		if _, err := exec.LookPath(tool); err != nil {
			if os.Getenv("KOTO_REQUIRE_E2FS") == "1" {
				t.Fatalf("%s missing and KOTO_REQUIRE_E2FS=1", tool)
			}
			t.Skipf("%s missing", tool)
		}
	}
	fcHarness(t)
	e := newImageTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.client = &ociimage.Client{Ctx: ctx}
	l1, b1 := testGzLayer("etc/os-release", "ubu")
	l2, b2 := testGzLayer("usr/bin/tool", "#!/bin/sh\n")
	e.pull = func(r ociimage.Ref, root string) (*ociimage.Image, error) {
		for i, l := range []ociimage.Layer{l1, l2} {
			p := ociimage.BlobPath(root, l.Digest)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, [][]byte{b1, b2}[i], 0o444); err != nil {
				return nil, err
			}
		}
		dg := ociimage.Sha([]byte("idx-l"))
		return &ociimage.Image{Digest: dg, Ref: r.Pinned(dg), Arch: "amd64", Created: time.Now(),
			Layers: []ociimage.Layer{l1, l2}}, nil
	}
	var log strings.Builder
	err := imageRun(e.imageEnv, "add", []string{"ubuntu"}, false, func(f string, a ...any) {
		line := fmt.Sprintf(f, a...)
		log.WriteString(line + "\n")
		if strings.Contains(line, "layer 1/2") && strings.Contains(line, "converted") {
			cancel() // Esc lands while layer 1 converts
		}
	})
	if !strings.Contains(log.String(), "converted") {
		t.Fatalf("layer build not reached (err %v):\n%s", err, log.String())
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("add cancelled mid-build = %v, want context.Canceled\n%s", err, log.String())
	}
	if strings.Contains(log.String(), "layer 2/2") {
		t.Fatalf("layer 2 built after the cancel:\n%s", log.String())
	}
	for i, l := range []ociimage.Layer{l1, l2} {
		if _, serr := os.Stat(ociimage.LayerFile(fcImageStore().Root, l.DiffID)); serr == nil {
			t.Fatalf("cancelled add left layer %d's file", i+1)
		}
	}
	c, lerr := fcImageStore().Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if _, ok := c.Names["ubuntu"]; ok {
		t.Fatal("cancelled add left a catalog entry")
	}
}
