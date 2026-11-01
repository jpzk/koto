package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCliDefaultIdentity pins which client identity each CLI path dials as
// (PR #92 round 7): `koto image` and top-level `koto spawn --image` default
// to the admin client tui (the Image rpc and image= are admin-only), an
// explicit KOTO_CLIENT wins, and `koto ctl` (incl. ctl spawn -image) keeps
// the agent default. The real entry points run in a re-exec'd child against
// an empty creds dir, so the identity shows up in the "no token" error; no
// daemon or PKI is needed.
func TestCliDefaultIdentity(t *testing.T) {
	if v := os.Getenv("KOTO_TEST_CLI_ENTRY"); v != "" {
		args := strings.Fields(os.Getenv("KOTO_TEST_CLI_ARGS"))
		switch v {
		case "image":
			imageCliMain(args)
		case "spawn":
			ctlSpawn(args, true)
		case "ctl":
			ctlCliMain(args)
		}
		os.Exit(0)
	}

	for _, tc := range []struct {
		name, entry, args, client string
		want                      string // identity in the error
		wantHint                  bool   // admin default: names KOTO_CLIENT + creds dir
	}{
		{"image-ls-default-tui", "image", "ls", "", "tui", true},
		{"image-ls-explicit-wins", "image", "ls", "ops", "ops", true},
		{"spawn-image-default-tui", "spawn", "g --image debian", "", "tui", true},
		{"spawn-image-explicit-wins", "spawn", "g --image debian", "agent", "agent", true},
		{"spawn-plain-stays-agent", "spawn", "g", "", "agent", false},
		{"ctl-list-default-agent", "ctl", "list", "", "agent", false},
		{"ctl-spawn-image-default-agent", "ctl", "spawn g -image debian", "", "agent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestCliDefaultIdentity$")
			env := []string{"KOTO_TEST_CLI_ENTRY=" + tc.entry, "KOTO_TEST_CLI_ARGS=" + tc.args,
				"KOTO_CREDS_DIR=" + creds, "KOTO_ADDR=127.0.0.1:1"}
			for _, e := range os.Environ() {
				if !strings.HasPrefix(e, "KOTO_") {
					env = append(env, e)
				}
			}
			if tc.client != "" {
				env = append(env, "KOTO_CLIENT="+tc.client)
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected a no-token failure, got success: %s", out)
			}
			s := string(out)
			if !strings.Contains(s, "token-"+tc.want) {
				t.Fatalf("want identity %q (token-%s) in error, got: %s", tc.want, tc.want, s)
			}
			if got := strings.Contains(s, "KOTO_CLIENT") && strings.Contains(s, creds); got != tc.wantHint {
				t.Fatalf("admin hint (KOTO_CLIENT + creds dir) present=%v want %v: %s", got, tc.wantHint, s)
			}
		})
	}

	// resolution helpers, no process needed
	t.Setenv("KOTO_CLIENT", "")
	if g := ctlClientName(imageCliDefaultIdentity); g != "tui" {
		t.Fatalf("koto image default identity = %q, want tui", g)
	}
	if g := ctlClientName(ctlDefaultIdentity); g != "agent" {
		t.Fatalf("koto ctl default identity = %q, want agent", g)
	}
	t.Setenv("KOTO_CLIENT", "ops")
	if g := ctlClientName(imageCliDefaultIdentity); g != "ops" {
		t.Fatalf("explicit KOTO_CLIENT lost: %q", g)
	}
}
