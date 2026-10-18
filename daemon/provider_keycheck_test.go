package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKey = "vk-SECRET-do-not-leak-0123456789abcdef"

// keyCheckServer stands in for a provider: it answers status, records the
// auth header it saw, and counts requests.
func keyCheckServer(t *testing.T, status int, delay time.Duration) (*httptest.Server, *atomic.Value, *atomic.Int32) {
	t.Helper()
	var seen atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		seen.Store(r.Method + " " + r.URL.Path + " auth=" + r.Header.Get("Authorization") + " xkey=" + r.Header.Get("x-api-key"))
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
		// A provider that echoes the key back must not leak it either.
		_, _ = w.Write([]byte(`{"error":"bad key ` + testKey + `"}`))
	}))
	t.Cleanup(srv.Close)
	oldV, oldM, oldC := veniceKeyCheckURL, minimaxKeyCheckURL, keyCheckHTTP
	veniceKeyCheckURL = func() string { return srv.URL + "/api/v1/api_keys/rate_limits" }
	minimaxKeyCheckURL = func() string { return srv.URL + "/anthropic/v1/models" }
	t.Cleanup(func() { veniceKeyCheckURL, minimaxKeyCheckURL, keyCheckHTTP = oldV, oldM, oldC })
	return srv, &seen, &hits
}

func TestProviderKeyCheckStatuses(t *testing.T) {
	cases := []struct {
		status int
		want   keyCheckResult
	}{
		{200, keyVerified},
		{401, keyRejected},
		{403, keyRejected},
		{402, keyUnverified},
		{429, keyUnverified},
		{500, keyUnverified},
	}
	for _, p := range []string{providerVenice, providerMinimax} {
		for _, c := range cases {
			_, seen, _ := keyCheckServer(t, c.status, 0)
			res, err := providerKeyCheck(context.Background(), p, testKey)
			if res != c.want {
				t.Errorf("%s HTTP %d: result %v, want %v (err %v)", p, c.status, res, c.want, err)
			}
			if c.want == keyVerified && err != nil {
				t.Errorf("%s HTTP %d: unexpected err %v", p, c.status, err)
			}
			if err != nil && strings.Contains(err.Error(), testKey) {
				t.Errorf("%s HTTP %d: key leaked into error: %v", p, c.status, err)
			}
			s, _ := seen.Load().(string)
			switch p {
			case providerVenice:
				if s != "GET /api/v1/api_keys/rate_limits auth=Bearer "+testKey+" xkey=" {
					t.Errorf("venice request = %q", s)
				}
			case providerMinimax:
				if s != "GET /anthropic/v1/models auth= xkey="+testKey {
					t.Errorf("minimax request = %q", s)
				}
			}
		}
	}
}

func TestProviderKeyCheckUnreachable(t *testing.T) {
	// Timeout: a server that never answers within the client's deadline.
	_, _, _ = keyCheckServer(t, 200, 5*time.Second)
	keyCheckHTTP = &http.Client{Timeout: 200 * time.Millisecond}
	res, err := providerKeyCheck(context.Background(), providerVenice, testKey)
	if res != keyUnverified || err == nil {
		t.Fatalf("timeout: result %v err %v, want unverified", res, err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("key leaked into timeout error: %v", err)
	}

	// Connection refused: a closed port.
	srv, _, _ := keyCheckServer(t, 200, 0)
	srv.Close()
	res, err = providerKeyCheck(context.Background(), providerMinimax, testKey)
	if res != keyUnverified || err == nil {
		t.Fatalf("refused: result %v err %v, want unverified", res, err)
	}
	if strings.Contains(err.Error(), testKey) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("refused error = %v", err)
	}
}

func TestProviderKeyCheckShape(t *testing.T) {
	_, _, hits := keyCheckServer(t, 200, 0)
	for _, k := range []string{"", "has space", "line\nbreak", "tab\there", "nul\x00x"} {
		res, err := providerKeyCheck(context.Background(), providerVenice, k)
		if res != keyRejected || err == nil {
			t.Errorf("%q: result %v err %v, want rejected", k, res, err)
		}
		if k != "" && strings.Contains(err.Error(), k) {
			t.Errorf("%q echoed in error %v", k, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("malformed keys reached the network (%d requests)", hits.Load())
	}
}

func TestKeyCheckScrub(t *testing.T) {
	if got := keyCheckScrub("x "+testKey+" y", testKey); strings.Contains(got, testKey) {
		t.Fatalf("scrub left key: %q", got)
	}
	if got := keyCheckScrub("abc", ""); got != "abc" {
		t.Fatalf("empty key scrub = %q", got)
	}
}

func keyStoreCtx(t *testing.T) (*authCtx, string) {
	t.Helper()
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "creds"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &authCtx{ui: testUI(""), state: state}, filepath.Join(state, "creds")
}

func TestProviderStoreKeyRejectedLeavesFileUntouched(t *testing.T) {
	for _, p := range []string{providerVenice, providerMinimax} {
		_, _, _ = keyCheckServer(t, 401, 0)
		ac, creds := keyStoreCtx(t)
		path := filepath.Join(creds, keyProviders[p].file)
		orig := []byte("the-old-working-key\n")
		if err := os.WriteFile(path, orig, 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		out := captureStdout(t, func() { err = providerStoreKey(context.Background(), ac, p, testKey, true) })
		if err == nil || !strings.Contains(err.Error(), "rejected") || !strings.Contains(err.Error(), "nothing was stored") {
			t.Fatalf("%s: err = %v", p, err)
		}
		if strings.Contains(err.Error(), testKey) || strings.Contains(out, testKey) {
			t.Fatalf("%s: key leaked (err %v, out %q)", p, err, out)
		}
		got, _ := os.ReadFile(path)
		if !bytes.Equal(got, orig) {
			t.Fatalf("%s: rejected key changed the file", p)
		}
	}
}

func TestProviderStoreKeyVerifiedWrites(t *testing.T) {
	for _, p := range []string{providerVenice, providerMinimax} {
		_, _, _ = keyCheckServer(t, 200, 0)
		ac, creds := keyStoreCtx(t)
		var err error
		out := captureStdout(t, func() { err = providerStoreKey(context.Background(), ac, p, testKey, true) })
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		path := filepath.Join(creds, keyProviders[p].file)
		got, _ := os.ReadFile(path)
		if string(got) != testKey {
			t.Fatalf("%s: stored %d bytes, want the key", p, len(got))
		}
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: mode %v", p, fi.Mode().Perm())
		}
		if strings.Contains(out, testKey) || !strings.Contains(out, "accepted") {
			t.Fatalf("%s: output %q", p, out)
		}
	}
}

func TestProviderStoreKeyUnreachableStoresWithWarning(t *testing.T) {
	srv, _, _ := keyCheckServer(t, 200, 0)
	srv.Close()
	ac, creds := keyStoreCtx(t)
	var err error
	out := captureStdout(t, func() { err = providerStoreKey(context.Background(), ac, providerVenice, testKey, true) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "could not verify") || strings.Contains(out, testKey) {
		t.Fatalf("output %q", out)
	}
	if got, _ := os.ReadFile(filepath.Join(creds, "venice.key")); string(got) != testKey {
		t.Fatal("unverified key was not stored")
	}
}

func TestProviderStoreKeyNoVerifySkipsRequest(t *testing.T) {
	_, _, hits := keyCheckServer(t, 401, 0)
	ac, creds := keyStoreCtx(t)
	var err error
	_ = captureStdout(t, func() { err = providerStoreKey(context.Background(), ac, providerMinimax, testKey, false) })
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("--no-verify made %d requests", hits.Load())
	}
	if got, _ := os.ReadFile(filepath.Join(creds, "minimax.key")); string(got) != testKey {
		t.Fatal("key not stored")
	}
}

// The wizard path runs the same check: a rejected key fails the login and
// writes nothing.
func TestWizardKeyLoginChecksKey(t *testing.T) {
	_, _, hits := keyCheckServer(t, 403, 0)
	state := t.TempDir()
	t.Setenv("KOTO_HOME", state)
	sc := newTestSC(testKey + "\n")
	var err error
	out := captureStdout(t, func() { err = veniceCredLogin(sc) })
	if err == nil || strings.Contains(err.Error(), testKey) || strings.Contains(out, testKey) {
		t.Fatalf("err %v out %q", err, out)
	}
	if hits.Load() != 1 {
		t.Fatalf("wizard made %d key checks, want 1", hits.Load())
	}
	if _, serr := os.Stat(filepath.Join(state, "creds", "venice.key")); !os.IsNotExist(serr) {
		t.Fatalf("rejected key was written (stat err %v)", serr)
	}
}

func TestProviderReadKeyFirstLine(t *testing.T) {
	ac, creds := keyStoreCtx(t)
	f := filepath.Join(creds, "in")
	os.WriteFile(f, []byte("  "+testKey+"  \n# comment\n"), 0o600)
	k, err := providerReadKey(ac, providerVenice, f, false)
	if err != nil || k != testKey {
		t.Fatalf("key %d bytes err %v", len(k), err)
	}
	os.WriteFile(f, []byte("\n"), 0o600)
	if _, err := providerReadKey(ac, providerVenice, f, false); err == nil {
		t.Fatal("empty file accepted")
	}
}
