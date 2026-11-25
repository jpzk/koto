package mcpgw

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// AuthStatus follows the last token exchange: ok after a good one,
// relogin_needed when the AS refuses the grant, refresh_failed when the
// exchange fails for any other reason.
func TestAuthStatusFollowsExchange(t *testing.T) {
	f := &fakeOAuth{clients: map[string]string{}, codes: map[string][2]string{},
		access: map[string]fakeTok{}, refresh: map[string]string{}}
	var down atomic.Bool
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" && down.Load() {
			http.Error(w, "AS down", http.StatusServiceUnavailable)
			return
		}
		f.serve(w, r)
	}))
	defer f.srv.Close()
	hc := &http.Client{Timeout: 5 * time.Second}

	// refresh_token grant: ok, then the AS forgets every refresh token
	o, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "authorization_code", Scope: "mcp:add"})
	must(t, err)
	gw, _, _ := oauthGateway(t, f, o)
	if s := gw.AuthStatus("adder"); s != AuthOK {
		t.Fatalf("before any exchange: %s", s)
	}
	if _, b := gwRPC(t, gw, "tools/call", map[string]any{"name": "add"}); !strings.Contains(b, "ok add") {
		t.Fatalf("first call: %s", b)
	}
	if s := gw.AuthStatus("adder"); s != AuthOK {
		t.Fatalf("after good exchange: %s", s)
	}
	f.mu.Lock()
	f.refresh = map[string]string{}
	f.access = map[string]fakeTok{}
	f.mu.Unlock()
	gwRPC(t, gw, "tools/call", map[string]any{"name": "add"})
	if s := gw.AuthStatus("adder"); s != AuthReloginNeeded {
		t.Fatalf("after invalid_grant: %s", s)
	}
	if s := gw.AuthStatus("nope"); s != AuthOK {
		t.Fatalf("unknown conn: %s", s)
	}

	// client_credentials: the AS is unreachable-ish (503) -> refresh_failed,
	// and a later good exchange heals it back to ok
	o2, err := Onboard(context.Background(), hc, f.mcpURL(), OnboardReq{Grant: "client_credentials", Scope: "mcp:add"})
	must(t, err)
	gw2, _, _ := oauthGateway(t, f, o2)
	down.Store(true)
	gwRPC(t, gw2, "tools/call", map[string]any{"name": "add"})
	if s := gw2.AuthStatus("adder"); s != AuthRefreshFailed {
		t.Fatalf("after 503: %s", s)
	}
	down.Store(false)
	if _, b := gwRPC(t, gw2, "tools/call", map[string]any{"name": "add"}); !strings.Contains(b, "ok add") {
		t.Fatalf("call after recovery: %s", b)
	}
	if s := gw2.AuthStatus("adder"); s != AuthOK {
		t.Fatalf("after recovery: %s", s)
	}
}

func TestAuthStatusOf(t *testing.T) {
	for err, want := range map[error]string{
		nil:                                      AuthOK,
		&asError{"token", 400, "invalid_grant"}:  AuthReloginNeeded,
		&asError{"token", 401, "invalid_client"}: AuthReloginNeeded,
		&asError{"token", 400, "unauthorized_client"}: AuthReloginNeeded,
		&asError{"token", 500, "server_error"}:        AuthRefreshFailed,
		errors.New("token: AS unreachable"):           AuthRefreshFailed,
		context.DeadlineExceeded:                      AuthRefreshFailed,
	} {
		if got := authStatusOf(err); got != want {
			t.Errorf("authStatusOf(%v) = %s, want %s", err, got, want)
		}
	}
	// the error text is unchanged by the typed error
	if e := (&asError{"token", 400, "invalid_grant"}).Error(); e != "token: HTTP 400 error=invalid_grant" {
		t.Fatal(e)
	}
}
