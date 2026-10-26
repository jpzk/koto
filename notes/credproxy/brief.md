# goal cred-proxy — universal credentials proxy (option A: MITM + placeholders)

Operator decision 2026-10-26: option A. Guests use unmodified tools (curl, git, gh, SDKs).
The guest only ever holds placeholder tokens (e.g. KOTO_CRED_<name>). A host-side proxy
terminates TLS with a per-install koto CA trusted in the guest, swaps placeholder -> real
secret ONLY when (group, dest host, header/location) matches policy, and re-encrypts upstream.

Repo: /workspace/koto. Existing LLM proxy: daemon/proxy*.go, guest net: daemon/fcnet.go.
BRANCHING RULE: never develop on main. Make a worktree off origin/develop, branch feat/cred-proxy.
Disk is 75% full — clean up worktrees/build junk you create.

## phases
1. DESIGN first: notes/credproxy/design.md + threat model (STRIDE-ish). Must cover:
   - CA lifecycle (per-install, private key host-only 0600, never in guest; leaf certs minted
     on demand + cached; short validity; name-constrained if feasible)
   - injection into guest trust store (rootfs/boot), env (HTTPS_PROXY / transparent redirect?)
   - policy format: secret name -> allowed groups, allowed hosts (exact, no wildcard by default),
     header name / scheme (Bearer, Basic, custom header); deny-by-default
   - placeholder in non-allowlisted host => NOT substituted, request blocked + logged
   - secret exfil vectors: reflection in responses (redact real secret in response bodies/headers
     back to guest), redirects to other hosts (re-check policy per hop, strip auth), query string,
     body substitution (default OFF), DNS rebinding / SNI vs Host mismatch, CONNECT to IP literals
   - pinned clients: passthrough (no MITM) for hosts with no policy
   - interplay with existing LLM proxy + network none/wan/lan/full
   - secret storage on host (file under /etc/koto, 0600), rotation, audit log w/o secret values
2. IMPLEMENT in Go, stdlib crypto/tls + net/http only. NO new third-party deps unless pinned and
   released >=6 weeks ago (check). No python/js anywhere.
3. TESTS: unit tests for policy matching, substitution, redaction, redirect handling,
   SNI/Host mismatch, IP-literal CONNECT. An e2e (bash) that boots a guest, runs curl to a
   local test HTTPS server with placeholder, asserts the server saw the real secret and the
   guest never can read it (grep guest fs/env/proc for the secret value == 0 hits).
4. Push branch + open PR -> develop. Do NOT merge, do NOT install on this box, do NOT release.
No ntfy (disabled). cs-notify only for blocked/finished.
