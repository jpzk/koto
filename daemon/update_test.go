package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// koto update runs the installer embedded in the binary, so the embedded copy
// must be the repository's install.sh byte for byte. The Makefile refreshes it
// before every build; this catches a `go build` that skipped the Makefile.
func TestEmbeddedInstallerMatchesTheRepoCopy(t *testing.T) {
	repo, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repo, embeddedInstaller) {
		t.Fatal("daemon/installer/install.sh differs from install.sh — run `cp install.sh daemon/installer/install.sh` (make koto does)")
	}
	// The flags koto update passes must exist in the installer it embeds.
	for _, flag := range []string{"--no-attach)", "--yes)", "--version)"} {
		if !bytes.Contains(embeddedInstaller, []byte(flag)) {
			t.Errorf("embedded installer lacks %s", strings.TrimSuffix(flag, ")"))
		}
	}
}

func TestUpdateLatestRelease(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		location string
		want     string
		wantErr  string
	}{
		{"redirect to a tag", http.StatusFound, "https://github.com/jpzk/koto/releases/tag/1.0.1", "1.0.1", ""},
		{"no release", http.StatusOK, "", "", "no redirect"},
		{"not a version", http.StatusFound, "https://github.com/jpzk/koto/releases/tag/nightly", "", "not a version"},
		{"unexpected target", http.StatusFound, "https://github.com/login", "", "unexpected redirect"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead || r.URL.Path != "/releases/latest" {
					t.Errorf("request %s %s, want HEAD /releases/latest", r.Method, r.URL.Path)
				}
				if c.location != "" {
					w.Header().Set("Location", c.location)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			got, err := updateLatestRelease(context.Background(), srv.Client(), srv.URL)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("got %q, %v; want an error containing %q", got, err, c.wantErr)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestUpdateDecide(t *testing.T) {
	cases := []struct {
		installed, target string
		explicit          bool
		update            bool
		says              string
	}{
		{"1.0.0", "1.0.1", false, true, "available"},
		{"1.0.0", "1.1.0", false, true, "available"},
		{"1.0.1", "1.0.1", false, false, "up to date"},
		// A build from main is current for its base release, not offered a
		// "downgrade" to it...
		{"1.0.0-6-gd5150df-dirty", "1.0.0", false, false, "development build"},
		// ...unless the release is asked for by name.
		{"1.0.0-6-gd5150df-dirty", "1.0.0", true, true, "replaces development build"},
		{"1.0.0-6-gd5150df-dirty", "1.0.1", false, true, "available"},
		{"1.1.0", "1.0.1", false, false, "newer than the latest"},
		{"1.1.0", "1.0.1", true, true, "is a downgrade"},
		{"d5150df", "1.0.1", false, true, "not a release build"},
		{"1.0.0", "nightly", true, false, "not a release version"},
	}
	for _, c := range cases {
		v := updateDecide(c.installed, c.target, c.explicit)
		if v.update != c.update || !strings.Contains(v.message, c.says) {
			t.Errorf("updateDecide(%q, %q, %v) = %+v; want update=%v and a message saying %q",
				c.installed, c.target, c.explicit, v, c.update, c.says)
		}
	}
}

func TestParseSemver(t *testing.T) {
	for in, want := range map[string]string{
		"1.0.0": "1.0.0", "v2.3.4": "2.3.4", "1.0.0-6-gabc-dirty": "1.0.0", "10.20.30+build": "10.20.30",
	} {
		v, ok := parseSemver(in)
		if !ok || v.String() != want {
			t.Errorf("parseSemver(%q) = %v, %v; want %s", in, v, ok, want)
		}
	}
	for _, in := range []string{"", "1.0", "1.0.0.0", "a.b.c", "-1.0.0", "nightly"} {
		if _, ok := parseSemver(in); ok {
			t.Errorf("parseSemver(%q) accepted a non-version", in)
		}
	}
}
