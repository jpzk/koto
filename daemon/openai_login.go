package main

// `koto openai-login` — connect the credential the proxy injects for groups
// on provider "openai". The counterpart of `koto claude-login`; see
// openai_auth.go for where the credential lives and why it never enters a
// guest.
//
// The subscription path is a device-code login: this prints a URL and a
// one-time code, the operator enters it in a browser on ANY machine, and the
// tokens land in <state>/creds/openai-auth.json (0600). Nothing in a microVM
// is involved and nothing needs a browser on the host. The daemon refreshes
// the access token itself before it expires, so this is run once, and again
// only if the refresh token is revoked.
//
// Like claude-login it restarts nothing: the proxy reads the credential per
// request.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"
)

func openaiLoginUsage() {
	fmt.Fprintln(os.Stderr, `usage: koto openai-login [flags]

Connects the OpenAI credential the daemon's proxy injects into turns of
groups configured with provider "openai" (Codex CLI in the guest).

Two ways in, both stored under <state>/creds at 0600:
  device   ChatGPT subscription, device-code login (default). Prints a URL
           and a one-time code to enter in a browser on any machine.
  api-key  an OpenAI platform API key (creds/openai-api-key).

The subscription wins when both exist. The daemon re-reads per request
and refreshes the subscription token itself; nothing is restarted.

flags:
  --status         show which credential would be sent; change nothing
  --method M       "device" or "api-key"
  --api-key-stdin  read the API key from stdin (implies --method api-key)
  --no-color       plain output (also honors NO_COLOR)`)
}

func openaiLoginFatal(code int, format string, a ...any) {
	fmt.Fprintf(os.Stderr, "koto openai-login: "+format+"\n", a...)
	os.Exit(code)
}

func openaiLoginMain(args []string) {
	fs := flag.NewFlagSet("openai-login", flag.ExitOnError)
	fs.Usage = openaiLoginUsage
	status := fs.Bool("status", false, "report only")
	method := fs.String("method", "device", "device | api-key")
	keyStdin := fs.Bool("api-key-stdin", false, "read the API key from stdin")
	noColor := fs.Bool("no-color", false, "disable color")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		openaiLoginFatal(2, "unexpected argument %q — this command takes flags only", fs.Arg(0))
	}
	if *keyStdin {
		*method = "api-key"
	}
	if *method != "device" && *method != "api-key" {
		openaiLoginFatal(2, "unknown --method %q (device | api-key)", *method)
	}

	state, why := authStateDir()
	// openai_auth.go resolves paths from HERE, as the daemon does; point it
	// at the state dir this command configures.
	HERE = state
	ac := &authCtx{ui: newSetupUI(false, *noColor), state: state}
	if *status {
		os.Exit(openaiReport(ac.ui))
	}
	ac.ui.info("configuring %s (%s)", state, why)
	if err := os.MkdirAll(ac.credsDir(), 0o700); err != nil {
		openaiLoginFatal(1, "%v", err)
	}
	if err := authTrustedCredsDir(ac); err != nil {
		openaiLoginFatal(1, "%v", err)
	}

	var err error
	if *method == "api-key" {
		err = openaiStoreKey(ac, *keyStdin)
	} else {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		err = openaiStoreDevice(ctx, ac)
	}
	if err != nil {
		openaiLoginFatal(1, "%v", err)
	}
	ac.ui.blank()
	os.Exit(openaiReport(ac.ui))
}

func openaiStoreDevice(ctx context.Context, ac *authCtx) error {
	f, err := openaiDeviceLogin(ctx, func(verifyURL, code string) {
		ac.ui.blank()
		ac.ui.info("1. open %s in a browser (any machine) and sign in to ChatGPT", verifyURL)
		ac.ui.info("2. enter the one-time code: %s   (expires in 15 minutes)", code)
		ac.ui.info("   only continue if YOU started this login")
		ac.ui.blank()
		ac.ui.info("waiting for approval…")
	})
	if err != nil {
		return err
	}
	if err := openaiWriteAuthFile(f); err != nil {
		return err
	}
	ac.ui.ok("stored in %s (0600); the daemon refreshes it before expiry", openaiAuthPath())
	return nil
}

func openaiStoreKey(ac *authCtx, fromStdin bool) error {
	var key string
	if fromStdin {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 8192))
		if err != nil {
			return fmt.Errorf("read key from stdin: %w", err)
		}
		key = strings.TrimSpace(string(b))
	} else {
		s, err := ac.ui.secret("OpenAI API key (sk-…)")
		if err != nil {
			return err
		}
		key = strings.TrimSpace(s)
	}
	if key == "" {
		return errors.New("no key given")
	}
	if !strings.HasPrefix(key, "sk-") {
		ac.ui.warn("that doesn't look like an OpenAI key, storing it anyway")
	}
	if err := pkiWriteFile(openaiAPIKeyPath(), []byte(key), 0o600); err != nil {
		return err
	}
	ac.ui.ok("stored in %s (0600)", openaiAPIKeyPath())
	return nil
}

// openaiReport says which credential the proxy would inject, redacted.
// Local only: it does not spend a request. It DOES NOT refresh either —
// refreshing is the daemon's job, and a second process rotating the
// single-use refresh token behind the daemon's back is how you get logged
// out.
func openaiReport(u *setupUI) int {
	f, ferr := openaiReadAuthFile()
	if ferr == nil && f.Tokens != nil && f.Tokens.AccessToken != "" {
		u.ok("ChatGPT subscription (OAuth) in %s", openaiAuthPath())
		if f.Tokens.AccountID != "" {
			u.info("account %s", authRedact(f.Tokens.AccountID))
		}
		if exp, ok := jwtExp(f.Tokens.AccessToken); ok {
			d := time.Until(exp)
			if d > 0 {
				u.info("access token expires in %s (the daemon refreshes it %s before)", d.Round(time.Minute), openaiRefreshWindow)
			} else {
				u.info("access token expired %s ago; the daemon refreshes on the next request", (-d).Round(time.Minute))
			}
		}
		if f.Tokens.RefreshToken == "" {
			u.warn("no refresh token: re-run `koto openai-login` when it expires")
		}
		if _, err := os.Stat(openaiAPIKeyPath()); err == nil {
			u.info("creds/openai-api-key also present (fallback, not sent)")
		}
		return 0
	}
	if b, err := os.ReadFile(openaiAPIKeyPath()); err == nil && strings.TrimSpace(string(b)) != "" {
		u.ok("OpenAI API key in %s (%s)", openaiAPIKeyPath(), authRedact(strings.TrimSpace(string(b))))
		return 0
	}
	if ferr != nil && !errors.Is(ferr, os.ErrNotExist) {
		u.warn("%v", ferr)
	}
	u.warn("no OpenAI credential: run `koto openai-login`")
	return 1
}
