package main

// `koto venice-login` and `koto minimax-login` — store the API key the proxy
// injects for groups on provider "venice" / "minimax", after checking it with
// the provider (provider_keycheck.go). The counterparts of `koto
// openai-login --method api-key`; the setup wizard runs the same code.
//
// Like the other logins it restarts nothing: the proxy reads the key from
// <state>/creds per request.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
)

func providerLoginUsage(provider string) func() {
	kp := keyProviders[provider]
	cmd := provider + "-login"
	return func() {
		fmt.Fprintf(os.Stderr, `usage: koto %s [flags]

Stores the %s key the daemon's proxy injects into turns of groups on
provider %q, in <state>/creds/%s (0600). The key never enters a guest.

Before anything is written the key is checked with one request that
requires auth. A key %s rejects (401/403) is refused and the file on disk
is left untouched. If %s cannot be reached the key is stored with a
warning. The daemon re-reads the key per request; nothing is restarted.

Without --key-file or --api-key-stdin the key is asked for on the terminal
(hidden). Create one at %s.

flags:
  --status         say whether a key is stored; change nothing, no request
  --key-file F     read the key from file F (first line, trimmed)
  --api-key-stdin  read the key from stdin
  --no-verify      store without checking it with %s
  --no-color       plain output (also honors NO_COLOR)
`, cmd, kp.label, provider, kp.file, kp.label, kp.label, kp.where, kp.label)
	}
}

func providerLoginMain(provider string, args []string) {
	cmd := provider + "-login"
	fatal := func(code int, format string, a ...any) {
		fmt.Fprintf(os.Stderr, "koto "+cmd+": "+format+"\n", a...)
		os.Exit(code)
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = providerLoginUsage(provider)
	status := fs.Bool("status", false, "report only")
	keyFile := fs.String("key-file", "", "read the key from this file")
	keyStdin := fs.Bool("api-key-stdin", false, "read the key from stdin")
	noVerify := fs.Bool("no-verify", false, "skip the key check")
	noColor := fs.Bool("no-color", false, "disable color")
	_ = fs.Parse(args)
	if fs.NArg() > 0 {
		fatal(2, "unexpected argument %q — this command takes flags only (the key goes in --key-file, stdin or the prompt, never argv)", fs.Arg(0))
	}
	if *keyFile != "" && *keyStdin {
		fatal(2, "--key-file and --api-key-stdin are mutually exclusive")
	}

	state, why := authStateDir()
	ac := &authCtx{ui: newSetupUI(false, *noColor), state: state}
	if *status {
		os.Exit(providerKeyReport(ac, provider))
	}
	ac.ui.info("configuring %s (%s)", state, why)

	key, err := providerReadKey(ac, provider, *keyFile, *keyStdin)
	if err != nil {
		fatal(1, "%v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := providerStoreKey(ctx, ac, provider, key, !*noVerify); err != nil {
		fatal(1, "%v", err)
	}
}

// providerReadKey takes the key from a file, stdin, or a hidden prompt.
// Only the first line of a file/stdin counts, so a trailing newline or a
// comment line below the key is harmless.
func providerReadKey(ac *authCtx, provider, keyFile string, fromStdin bool) (string, error) {
	var r io.Reader
	switch {
	case keyFile != "":
		f, err := os.Open(keyFile)
		if err != nil {
			return "", fmt.Errorf("key file: %w", err)
		}
		defer f.Close()
		r = f
	case fromStdin:
		r = os.Stdin
	default:
		return providerPromptKey(ac, provider)
	}
	b, err := io.ReadAll(io.LimitReader(r, 8192))
	if err != nil {
		return "", fmt.Errorf("read key: %w", err)
	}
	line, _, _ := strings.Cut(string(b), "\n")
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("no key given")
	}
	return key, nil
}

// providerKeyReport says whether a key is stored. Local only, no request, and
// it prints the length, never any part of the key.
func providerKeyReport(ac *authCtx, provider string) int {
	kp := keyProviders[provider]
	path := filepath.Join(ac.credsDir(), kp.file)
	b, err := os.ReadFile(path)
	if err == nil && strings.TrimSpace(string(b)) != "" {
		ac.ui.ok("%s key in %s (%d characters)", kp.label, path, len(strings.TrimSpace(string(b))))
		return 0
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		ac.ui.warn("%v", err)
	}
	ac.ui.warn("no %s key: run `koto %s-login`", kp.label, provider)
	return 1
}
