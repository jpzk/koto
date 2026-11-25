package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"koto/mcpgw"
	"koto/mcpreg"
	"koto/ociimage"
)

// The onboarding (discovery / DCR / first grant) client must not follow
// redirects and must refuse non-public targets for https endpoints.
func TestMcpOnboardClientNoRedirectPublicOnly(t *testing.T) {
	sinkHit := false
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sinkHit = true }))
	defer sink.Close()
	src := httptest.NewServer(http.RedirectHandler(sink.URL+"/token", http.StatusTemporaryRedirect))
	defer src.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tls.Close()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")

	old := mcpOAuthOnboard
	t.Cleanup(func() { mcpOAuthOnboard = old })
	called := false
	mcpOAuthOnboard = func(ctx context.Context, hc *http.Client, _ string, _ mcpgw.OnboardReq) (*mcpreg.OAuth, error) {
		called = true
		resp, err := hc.Get(src.URL)
		if err != nil {
			t.Fatalf("redirect source: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect || sinkHit {
			t.Fatalf("status=%d sinkHit=%v: onboarding client followed a redirect", resp.StatusCode, sinkHit)
		}
		if _, err := hc.Get(tls.URL); !errors.Is(err, ociimage.ErrBlockedAddr) {
			t.Fatalf("https loopback: err=%v, want ErrBlockedAddr", err)
		}
		return nil, errors.New("stop")
	}
	_, _ = mcpOnboard("http://127.0.0.1:7/mcp", `{"grant":"client_credentials"}`)
	if !called {
		t.Fatal("onboard hook not reached")
	}
}
