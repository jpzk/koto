package ociimage

import (
	"strings"
	"testing"
)

func TestKnownImages(t *testing.T) {
	want := []KnownImage{
		{Name: FedoraName, Label: "fedora (default)", Ref: FedoraRef},
		{Name: "debian", Label: "debian", Ref: "docker.io/library/debian:latest"},
		{Name: "ubuntu", Label: "ubuntu", Ref: "docker.io/library/ubuntu:latest"},
		{Name: "kali", Label: "kali", Ref: "docker.io/kalilinux/kali-rolling:latest"},
	}
	got := Known()
	if len(got) != len(want) {
		t.Fatalf("Known() = %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Known()[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, k := range got {
		if !ValidName(k.Name) {
			t.Errorf("%q is not a valid catalog name", k.Name)
		}
		if k.Name == DefaultName {
			t.Errorf("the legacy built-in %q must not be offered by a picker", k.Name)
		}
		if _, err := ParseRef(k.Ref); err != nil {
			t.Errorf("%s: ParseRef(%q): %v", k.Name, k.Ref, err)
		}
	}
	// The copy is a copy: a caller can't edit the list.
	got[1].Ref = "evil/x:latest"
	if r, _ := KnownRef("debian"); r != want[1].Ref {
		t.Fatalf("Known() returned the backing array: KnownRef(debian) = %q", r)
	}
}

// The default base is pinned by digest, never a moving tag, and is the
// fedora:44 digest the guest.img recipe and Dockerfile.rootfs build FROM.
func TestFedoraRefPinned(t *testing.T) {
	r, err := ParseRef(FedoraRef)
	if err != nil {
		t.Fatalf("ParseRef(FedoraRef): %v", err)
	}
	const dg = "sha256:be9d65e2344d805cc11114319c685ecaa96b6d9b4350a0a6460cdb931babbd19"
	if !strings.HasSuffix(FedoraRef, "@"+dg) || r.Digest != dg || r.Tag != "" {
		t.Fatalf("FedoraRef = %q (parsed %v), want pinned @%s", FedoraRef, r, dg)
	}
	if (&Catalog{}).DefaultImage() != FedoraName {
		t.Fatalf("empty catalog default = %q, want %q", (&Catalog{}).DefaultImage(), FedoraName)
	}
	if (&Catalog{Default: "debian"}).DefaultImage() != "debian" {
		t.Fatal("an explicit catalog default must win")
	}
}

func TestKnownRef(t *testing.T) {
	for _, c := range []struct {
		name, ref string
		ok        bool
	}{
		{FedoraName, FedoraRef, true},
		{"debian", "docker.io/library/debian:latest", true},
		{"ubuntu", "docker.io/library/ubuntu:latest", true},
		{"kali", "docker.io/kalilinux/kali-rolling:latest", true},
		{DefaultName, "", false}, // built-in: nothing to pull
		{"default", "", false},   // a label, not a name
		{"alpine", "", false},
		{"", "", false},
	} {
		ref, ok := KnownRef(c.name)
		if ref != c.ref || ok != c.ok {
			t.Errorf("KnownRef(%q) = %q, %v; want %q, %v", c.name, ref, ok, c.ref, c.ok)
		}
	}
}
