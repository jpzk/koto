package ociimage

import "testing"

func TestKnownImages(t *testing.T) {
	want := []KnownImage{
		{Name: DefaultName, Label: "default"},
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
		if k.Ref == "" {
			continue
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

func TestKnownRef(t *testing.T) {
	for _, c := range []struct {
		name, ref string
		ok        bool
	}{
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
