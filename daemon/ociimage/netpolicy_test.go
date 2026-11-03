package ociimage

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBlockedAddr(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "127.9.9.9", "10.1.2.3", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0", "255.255.255.255", "224.0.0.1", "::1", "::",
		"fe80::1", "fd00:ec2::254", "fc00::1", "::ffff:127.0.0.1", "::ffff:169.254.169.254",
		"64:ff9b::a9fe:a9fe", "64:ff9b::7f00:1", "198.18.0.1", "ff02::1"} {
		if !BlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("BlockedAddr(%s) = false, want refused", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "104.16.0.1", "2606:4700::1111", "64:ff9b::808:808", "::ffff:1.1.1.1"} {
		if BlockedAddr(netip.MustParseAddr(s)) {
			t.Errorf("BlockedAddr(%s) = true, want allowed", s)
		}
	}
}

// sideServer is a second https endpoint (token realm / redirect target) on a
// loopback port the policy does NOT allow unless the test says so.
type sideServer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits int
	auth []string
}

func newSide(t *testing.T, body string) *sideServer {
	s := &sideServer{}
	s.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		s.auth = append(s.auth, r.Header.Get("Authorization"))
		s.mu.Unlock()
		w.Write([]byte(body))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *sideServer) host() string { return strings.TrimPrefix(s.srv.URL, "https://") }

func pullErr(t *testing.T, f *fakeRegistry, c *Client) error {
	t.Helper()
	r, _ := ParseRef(f.host() + "/test/img:a")
	_, err := c.Pull(r, t.TempDir())
	return err
}

func TestPolicyRefusesInternalTokenRealm(t *testing.T) {
	side := newSide(t, `{"token":"stolen"}`)
	for _, c := range []struct{ name, realm, want string }{
		{"loopback-ip", side.srv.URL + "/token", "non-public"},
		{"loopback-dns", "https://localhost:" + side.host()[strings.LastIndex(side.host(), ":")+1:] + "/token", "non-public"},
		{"metadata", "https://169.254.169.254/latest/meta-data/", "non-public"},
		{"private", "https://10.0.0.1/token", "non-public"},
		{"ula", "https://[fd00::1]/token", "non-public"},
		{"http", "http://auth.example.com/token", "bad token realm"},
		{"userinfo", "https://u:p@auth.example.com/token", "bad token realm"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeRegistry(t)
			f.token = "tok"
			f.realm = c.realm
			f.addImage("a", time.Now(), mkTar(t, []tfile{{name: "x", body: "y"}}))
			cl := f.client(nil)
			cl.Tries = 1
			err := pullErr(t, f, cl)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("realm %s: err = %v, want %q", c.realm, err, c.want)
			}
			if c.want == "non-public" && !errors.Is(err, ErrBlockedAddr) {
				t.Fatalf("realm %s: err not ErrBlockedAddr: %v", c.realm, err)
			}
		})
	}
	if side.hits != 0 {
		t.Fatalf("internal realm was contacted %d times", side.hits)
	}
}

func TestPolicyRedirects(t *testing.T) {
	l := mkTar(t, []tfile{{name: "x", body: "y"}})
	setup := func(t *testing.T) (*fakeRegistry, string) {
		f := newFakeRegistry(t)
		_, ls := f.addImage("a", time.Now(), l)
		return f, "/v2/test/img/blobs/" + ls[0]
	}
	t.Run("internal-target-refused", func(t *testing.T) {
		side := newSide(t, "nope")
		f, p := setup(t)
		f.redirect[p] = side.srv.URL + "/blob"
		cl := f.client(nil)
		cl.Tries = 1
		if err := pullErr(t, f, cl); !errors.Is(err, ErrBlockedAddr) {
			t.Fatalf("redirect to internal: %v", err)
		}
		if side.hits != 0 {
			t.Fatal("internal redirect target contacted")
		}
	})
	t.Run("metadata-target-refused", func(t *testing.T) {
		f, p := setup(t)
		f.redirect[p] = "https://169.254.169.254/latest/"
		cl := f.client(nil)
		cl.Tries = 1
		if err := pullErr(t, f, cl); !errors.Is(err, ErrBlockedAddr) {
			t.Fatalf("redirect to metadata: %v", err)
		}
	})
	t.Run("http-refused", func(t *testing.T) {
		f, p := setup(t)
		f.redirect[p] = "http://cdn.example.com/blob"
		cl := f.client(nil)
		cl.Tries = 1
		if err := pullErr(t, f, cl); err == nil || !strings.Contains(err.Error(), "non-https redirect") {
			t.Fatalf("http redirect: %v", err)
		}
	})
	t.Run("loop-capped", func(t *testing.T) {
		f, p := setup(t)
		f.redirect[p] = f.srv.URL + p // to itself, forever
		cl := f.client(nil)
		cl.Tries = 1
		if err := pullErr(t, f, cl); err == nil || !strings.Contains(err.Error(), "redirects") {
			t.Fatalf("redirect loop: %v", err)
		}
		if f.hits[p] > maxRedirects+1 {
			t.Fatalf("redirect loop followed %d times", f.hits[p])
		}
	})
	t.Run("cross-host-drops-token", func(t *testing.T) {
		f, p := setup(t)
		f.token = "registry-secret"
		blob := f.docs[strings.TrimPrefix(p, "/v2/test/img/blobs/")]
		side := newSide(t, string(blob))
		f.redirect[p] = side.srv.URL + "/cdn"
		cl := f.client(nil)
		cl.testTrust(f.pool(), func(ap netip.AddrPort) bool { return f.allow(ap) || ap.String() == side.host() })
		if err := pullErr(t, f, cl); err != nil {
			t.Fatalf("allowed cross-host redirect: %v", err)
		}
		if side.hits != 1 || side.auth[0] != "" {
			t.Fatalf("cdn hits=%d auth=%q: registry token leaked cross-host", side.hits, side.auth)
		}
	})
}

func TestBodyStallTimesOut(t *testing.T) {
	f := newFakeRegistry(t)
	_, ls := f.addImage("a", time.Now(), mkTar(t, []tfile{{name: "x", body: strings.Repeat("z", 4096)}}))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	side := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(side.Close)
	f.redirect["/v2/test/img/blobs/"+ls[0]] = side.URL + "/slow"
	cl := f.client(nil)
	cl.Tries = 1
	cl.stall = 200 * time.Millisecond
	cl.testTrust(f.pool(), func(ap netip.AddrPort) bool { return true })
	t0 := time.Now()
	err := pullErr(t, f, cl)
	if err == nil || !errors.Is(err, errStall) || !transient(err) {
		t.Fatalf("stalled body: err = %v (want stall timeout, transient)", err)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("stall took %s", d)
	}
}
