// mcp-adder — a real (non-mock) MCP server for koto's MCP gateway tests.
//
// Streamable-HTTP MCP at <base>/mcp with tools add, multiply (scope
// mcp:add) and reset_counter (scope mcp:admin). Auth modes:
//
//	oauth  OAuth 2.1 resource server + bundled TEST authorization server
//	       (DCR, auth code + PKCE S256 auto-approve, client_credentials,
//	       refresh rotation, RFC 8707 resource -> aud)
//	pat    static bearer read from a 0600 file
//	both   either
//
// Logs are JSON lines; no token or secret is ever logged.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func readPAT(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s: mode %v, want 0600 (no group/other access)", path, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(b))
	if len(p) < 16 {
		return "", errors.New("PAT too short (<16 chars)")
	}
	return p, nil
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("mcp-adder", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", "127.0.0.1:8931", "listen address")
	base := fs.String("base-url", "", "public base URL / AS issuer (default http://<listen>)")
	mode := fs.String("mode", "oauth", "auth mode: oauth | pat | both")
	patFile := fs.String("pat-file", "", "file (0600) holding the static bearer for -mode pat|both")
	patScopes := fs.String("pat-scopes", scopeAdd, "scopes carried by the PAT")
	ttl := fs.Duration("token-ttl", 5*time.Minute, "access token lifetime")
	rttl := fs.Duration("refresh-ttl", 24*time.Hour, "refresh token lifetime")
	maxScope := fs.String("max-scope", scopeAdd+" "+scopeAdmin, "scope ceiling for DCR clients")
	extra := fs.String("extra-resource", "", "comma list of other audiences the TEST AS may mint for")
	hooks := fs.Bool("test-hooks", false, "expose /test/stats and /test/expire (loopback tests only)")
	origins := fs.String("allowed-origins", "", "comma list of allowed Origin headers")
	logPath := fs.String("log", "", "JSON-lines log file (default stderr)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch *mode {
	case "oauth", "pat", "both":
	default:
		return fmt.Errorf("bad -mode %q", *mode)
	}
	cfg := Config{BaseURL: *base, Mode: *mode, PATScopes: splitList(*patScopes), TokenTTL: *ttl,
		RefreshTTL: *rttl, MaxScope: splitList(*maxScope), ExtraResources: splitList(*extra),
		TestHooks: *hooks, AllowedOrigins: splitList(*origins)}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://" + *listen
	}
	if *mode != "oauth" {
		if *patFile == "" {
			return errors.New("-mode pat|both needs -pat-file")
		}
		p, err := readPAT(*patFile)
		if err != nil {
			return err
		}
		cfg.PAT = p
	}
	var lw io.Writer = stderr
	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		lw = f
	}
	s, err := NewServer(cfg, lw)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.logEv("listening", "addr", ln.Addr().String(), "base_url", cfg.BaseURL, "resource", s.resource,
		"mode", cfg.Mode, "test_hooks", cfg.TestHooks, "token_ttl", cfg.TokenTTL.String(), "pid", os.Getpid())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(sc)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	s.logEv("stopped")
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "mcp-adder:", err)
		os.Exit(1)
	}
}
