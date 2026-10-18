package main

// Key check for the API-key providers (venice, minimax): before a key is
// written to <state>/creds, spend ONE cheap request that requires auth and
// see whether the provider accepts it. A typo'd or revoked key is refused
// and nothing is written, so the key that is already on disk (and working)
// is never replaced by a broken one. If the provider cannot be reached the
// key is stored anyway, with a warning: an offline host must still be able
// to configure itself.
//
// Endpoints were chosen because a bogus key gets a 401 on them (checked
// against the live APIs, 2026-10-18), not merely because they exist:
//   venice   GET /api/v1/api_keys/rate_limits  (/models is public: 200 for
//            any key, so it proves nothing)
//   minimax  GET /anthropic/v1/models with x-api-key, the route the proxy
//            forwards. /v1/token_plan/remains answers HTTP 200 with
//            base_resp 1004 for a bogus key, so its status code proves
//            nothing either.
// The check runs on the host against the real provider. It deliberately
// ignores KOTO_UPSTREAM: an outer bridge would accept the sentinel, not the
// key being checked.
//
// The key travels only in a request header. Errors carry the provider, the
// status code or the transport error, never the key; keyCheckScrub removes
// it anyway in case a lower layer ever echoes it.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type keyCheckResult int

const (
	keyVerified   keyCheckResult = iota // provider accepted the key
	keyRejected                         // 401/403: the key is wrong; store nothing
	keyUnverified                       // could not tell (network, 5xx, 429, ...); store with a warning
)

// keyCheckTimeout bounds the whole check; a login must not hang on a dead
// network.
const keyCheckTimeout = 15 * time.Second

// vars only so tests can point them at an httptest server.
var (
	keyCheckHTTP       = &http.Client{Timeout: keyCheckTimeout}
	veniceKeyCheckURL  = func() string { return veniceUpstream + "/api/v1/api_keys/rate_limits" }
	minimaxKeyCheckURL = func() string { return minimaxUpstream + "/anthropic/v1/models" }
)

// keyProvider describes one API-key provider for the login and wizard paths.
type keyProvider struct {
	name    string // provider id: "venice" | "minimax"
	label   string // human name
	file    string // under <state>/creds
	where   string // where to create a key
	prompt  string // hidden-prompt label
	request func(ctx context.Context, key string) (*http.Request, error)
}

var keyProviders = map[string]keyProvider{
	providerVenice: {
		name: providerVenice, label: "Venice", file: "venice.key",
		where:  "https://venice.ai/settings/api",
		prompt: "Venice API key",
		request: func(ctx context.Context, key string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, veniceKeyCheckURL(), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+key)
			return req, nil
		},
	},
	providerMinimax: {
		name: providerMinimax, label: "MiniMax", file: "minimax.key",
		where:  "https://platform.minimax.io (Subscription Key for the M Plan, or an API key)",
		prompt: "MiniMax key",
		request: func(ctx context.Context, key string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, minimaxKeyCheckURL(), nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
			return req, nil
		},
	},
}

// keyCheckScrub removes key from s. Belt and braces: nothing below is known
// to echo a header value, but an error string is exactly where a key would
// leak into a log.
func keyCheckScrub(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[redacted]")
}

// keyCheckShape rejects what cannot be a key before it goes anywhere: empty,
// or carrying whitespace/control characters (a pasted line break, a stray
// tab), which would also be an invalid header value.
func keyCheckShape(key string) error {
	if key == "" {
		return errors.New("no key given")
	}
	if len(key) > 4096 {
		return errors.New("key is implausibly long (over 4096 bytes)")
	}
	for _, r := range key {
		if r <= ' ' || r == 0x7f {
			return errors.New("key contains whitespace or control characters")
		}
	}
	return nil
}

// providerKeyCheck asks the provider whether key is valid. The error explains
// a rejection or an unverified result and never contains the key.
func providerKeyCheck(ctx context.Context, provider, key string) (keyCheckResult, error) {
	kp, ok := keyProviders[provider]
	if !ok {
		return keyUnverified, fmt.Errorf("no key check for provider %q", provider)
	}
	if err := keyCheckShape(key); err != nil {
		return keyRejected, err
	}
	ctx, cancel := context.WithTimeout(ctx, keyCheckTimeout)
	defer cancel()
	req, err := kp.request(ctx, key)
	if err != nil {
		return keyUnverified, errors.New(keyCheckScrub(fmt.Sprintf("%s: build request: %v", kp.label, err), key))
	}
	resp, err := keyCheckHTTP.Do(req)
	if err != nil {
		return keyUnverified, errors.New(keyCheckScrub(fmt.Sprintf("%s unreachable: %v", kp.label, err), key))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return keyVerified, nil
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return keyRejected, fmt.Errorf("%s rejected the key (HTTP %d)", kp.label, resp.StatusCode)
	default:
		return keyUnverified, fmt.Errorf("%s answered HTTP %d to the key check", kp.label, resp.StatusCode)
	}
}

// providerStoreKey checks key (unless verify is false) and writes it to
// <state>/creds/<file> at 0600. On rejection nothing is written. Shared by
// `koto venice-login` / `koto minimax-login` and the setup wizard.
func providerStoreKey(ctx context.Context, ac *authCtx, provider, key string, verify bool) error {
	kp, ok := keyProviders[provider]
	if !ok {
		return fmt.Errorf("unknown key provider %q", provider)
	}
	if err := keyCheckShape(key); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if verify {
		ac.ui.info("checking the key with %s…", kp.label)
		res, err := providerKeyCheck(ctx, provider, key)
		switch res {
		case keyVerified:
			ac.ui.ok("%s accepted the key", kp.label)
		case keyRejected:
			return fmt.Errorf("%v; nothing was stored", err)
		default:
			ac.ui.warn("could not verify the key: %v", err)
			ac.ui.warn("storing it anyway; turns will fail if it is wrong")
		}
	} else {
		ac.ui.warn("--no-verify: storing the key without checking it")
	}
	if err := os.MkdirAll(ac.credsDir(), 0o700); err != nil {
		return fmt.Errorf("creds dir: %w", err)
	}
	if err := authTrustedCredsDir(ac); err != nil {
		return err
	}
	path := filepath.Join(ac.credsDir(), kp.file)
	if err := pkiWriteFile(path, []byte(key), 0o600); err != nil {
		return err
	}
	ac.ui.ok("stored in %s (0600)", path)
	return nil
}

// providerPromptKey asks for the key on the terminal, hidden.
func providerPromptKey(ac *authCtx, provider string) (string, error) {
	kp := keyProviders[provider]
	ac.ui.info("create a key at %s", kp.where)
	s, err := ac.ui.secret(kp.prompt)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(s), nil
}
