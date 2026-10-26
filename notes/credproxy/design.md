# koto universal credentials proxy — design + threat model

Status: DESIGN (iter 1). Option A (operator decision 2026-10-26): MITM CA +
placeholder substitution. Guests run unmodified tools; they only ever hold
placeholder tokens. A host-side proxy terminates TLS with a per-install koto
CA (trusted in the guest), swaps placeholder → real secret only when
(group, dest host, header) matches policy, then re-encrypts upstream.

Scope of this doc: architecture + STRIDE-ish threat model. Implementation
follows on branch feat/cred-proxy.

## 0. goals / non-goals

Goals:
- Unmodified guest tooling (curl, git, gh, language SDKs) can authenticate to
  a fixed set of upstreams without the real secret ever existing in the guest.
- Deny-by-default: a placeholder is substituted ONLY for an explicitly
  allowlisted (group, host, header) tuple.
- The real secret never reaches the guest: not in request echoes, response
  bodies/headers, redirects, errors, or proxy logs.

Non-goals:
- Not a general forward proxy for arbitrary TLS (that stays with fcnet.go /
  the L7 LLM proxy). cred-proxy only MITMs hosts that have a policy; everything
  else is CONNECT-tunnelled untouched (passthrough) so pinned clients work.
- Not a secret manager / vault. Secrets are provisioned onto the host out of
  band; this component only stores+injects them.
- No body substitution by default (OFF; opt-in per policy, see §6).

## 1. architecture

```
guest tool (HTTPS_PROXY=http://cs_host:PORT, placeholder in Authorization)
   │  CONNECT api.example.com:443
   ▼
cred-proxy (host, in daemon)                          policy lookup
   ├─ host has policy?  no ──▶ raw TCP tunnel (passthrough, no MITM)
   └─ yes ──▶ terminate TLS with koto-CA leaf for that host (MITM)
                 │  decrypt request
                 │  substitute placeholder→secret iff policy matches (§4)
                 │  re-encrypt to real upstream (verify upstream cert vs system roots)
                 │  decrypt response, REDACT any secret bytes (§5)
                 ▼  re-encrypt response to guest
```

Placement: new package `daemon/credproxy/` + wiring in daemon. It listens on a
vsock/loopback port analogous to the existing L7 proxy. Guests reach the host
as `cs_host`/`cs_host_go` (koto-net); the proxy binds where guest egress can
reach it. The existing LLM proxy (proxy.go) is unchanged; cred-proxy is a
separate listener with its own policy so the Anthropic/OpenAI relay logic is
not entangled.

Guest config: `HTTPS_PROXY`/`https_proxy` (+ `HTTP_PROXY`) point at cred-proxy.
Explicit proxy (CONNECT) chosen over transparent REDIRECT because it is
portable, needs no iptables in the guest, and gives us the intended SNI/Host
from the CONNECT authority for cross-checking (§7). Transparent interception is
a possible later profile but out of scope here.

## 2. CA lifecycle & key custody  [THREAT: Spoofing, Info-disclosure, Elevation]

- Per-install root CA generated on first daemon start: P-256 ECDSA (stdlib
  crypto/ecdsa + crypto/x509), `IsCA=true`, `MaxPathLen=0`, validity ~10y.
- Root PRIVATE key lives ONLY on host at `/etc/koto/credproxy/ca.key`, mode
  0600, owner = daemon user. NEVER copied into any guest, never logged, never
  served over any port. Only the CA CERT (public) is injected into guests.
- Leaf certs are minted on demand per dest host, signed by the root, cached in
  memory (and optionally `/etc/koto/credproxy/leaf-cache/` 0600) with SHORT
  validity (≤24h) and the dest host in SAN. Cache keyed by host; evicted on
  expiry. Minting uses a fresh leaf key per host (not the CA key).
- NAME CONSTRAINTS: if feasible, the root carries
  `PermittedDNSDomains` = the set of policy hosts' registrable domains, so even
  if the CA cert leaked into a guest it cannot mint trusted certs for arbitrary
  sites. Fallback if a client ignores name constraints: the leaf cache only
  ever mints for hosts that have a policy (passthrough otherwise), so the
  blast radius of the trusted CA is still limited operationally. Documented as
  defense-in-depth, not sole control.
- Trust injection into guest: the CA cert (DER/PEM) is written into the guest
  rootfs trust store at build/boot time (e.g. appended to the system CA bundle
  the guest image uses, + `SSL_CERT_FILE`/`GIT_SSL_CAINFO`/`NODE_EXTRA_CA_CERTS`
  env for tools that read their own bundle). This is cert-only; no private
  material crosses the boundary.
- Rotation: regenerate root → re-inject cert into guest trust store on next
  boot; leaf cache flushed. Old CA key shredded (overwrite+unlink). Rotation is
  operator-triggered; audit-logged (event only, no key bytes).

## 3. secret storage on host  [THREAT: Info-disclosure, Tampering]

- Secrets stored under `/etc/koto/credproxy/secrets/` (or a single
  `secrets.json`), mode 0600, daemon-user owned, never world/group readable.
- In-memory the secret is held as `[]byte` and zeroed on reload where possible;
  never placed in Go string pools that outlive need when avoidable.
- Audit log (`/etc/koto/credproxy/audit.log`, 0600): records
  timestamp, group, dest host, secret NAME, decision (allow/deny), match
  reason. NEVER the secret VALUE or placeholder→value mapping. Redaction of the
  secret is enforced on the logging path itself (§5 redactor reused).

## 4. policy format & matching  [THREAT: Elevation, Spoofing — deny-by-default]

Policy is host-side config (`/etc/koto/credproxy/policy.json`, 0600). Per
secret:
- `name`: placeholder is `KOTO_CRED_<name>` (guest holds this literal string).
- `groups`: exact list of group names allowed to use this secret (no wildcard).
- `hosts`: EXACT hostnames (no wildcard by default; an opt-in explicit
  `*.suffix` allowed but discouraged and flagged in audit).
- `header`: header name + scheme — `Authorization: Bearer`, `Authorization:
  Basic`, or a custom header (e.g. `X-Api-Key`). Only this header is rewritten.
- `substitute_in`: default `["header"]`. `query`/`body` are opt-in (§6),
  default OFF.

Matching rule (ALL must hold or request is BLOCKED + audited as deny):
1. Request carries a known placeholder string.
2. Requesting group ∈ policy.groups for that placeholder's secret.
3. Dest host (from CONNECT authority, cross-checked vs SNI vs Host, §7) ∈
   policy.hosts EXACTLY.
4. The placeholder appears in an allowed location (header by default).
Deny-by-default: no matching policy ⇒ placeholder is NOT substituted and the
request is blocked (not forwarded with a dangling placeholder, not forwarded
stripped silently) — blocked + logged, so misuse is visible.

Placeholder-on-wrong-host: criterion-3 case. If a request to a non-allowlisted
host contains `KOTO_CRED_x`, the proxy does NOT substitute and BLOCKS the
request (prevents using a placeholder as an oracle / accidental leak of which
hosts are allowlisted via differential behavior is accepted: block is uniform).

## 5. response reflection / redaction  [THREAT: Info-disclosure]

After upstream responds, before relaying to guest, scan response for the real
secret VALUE and replace any occurrence with the placeholder (or a fixed
`[REDACTED]`). Covers:
- Response headers (e.g. upstream echoes auth, sets a cookie derived from it).
- Response body: streamed scan with a carry buffer sized ≥ secret length so a
  secret split across chunk boundaries is still caught (overlap window =
  len(secret)-1). Content-Length adjusted / switched to chunked if size
  changes.
- Error/redirect bodies too (same path).
The redactor is the single chokepoint also used by the audit logger (§3) so no
code path can emit the secret. Multiple secrets: redact all known values.

## 6. query-string / body substitution  [THREAT: Info-disclosure]

Default OFF. Rationale: substituting in bodies/queries means the proxy must
parse/modify arbitrary payloads and risks partial matches, encoding issues, and
Content-Length drift → higher leak surface. When a policy opts in
(`substitute_in` includes `query`/`body`), substitution is exact-string only,
length-delta handled, and the same redactor guards responses. Documented as
higher-risk, per-policy, audited.

## 7. SNI / Host / CONNECT-authority mismatch  [THREAT: Spoofing, rebinding]

Three names exist: CONNECT authority (what the client asked to reach), TLS SNI
(ClientHello), and HTTP Host header (inside the tunnel). Rules:
- The policy host decision uses the CONNECT authority host.
- SNI MUST equal the CONNECT authority host; mismatch ⇒ block (prevents a
  client claiming CONNECT good.com but TLS-handshaking evil.com to borrow a
  leaf / confuse policy).
- Host header MUST equal the CONNECT authority host for substitution to occur;
  a differing Host ⇒ no substitution + block (prevents Host-confusion smuggling
  the secret to an allowlisted host record while really talking to another).
- Upstream dial target = the CONNECT authority host resolved at request time;
  upstream TLS verified against SYSTEM roots (not our CA) with ServerName =
  that host. DNS rebinding: we resolve + dial the same name we validated;
  since upstream cert is checked against public roots for that exact name, a
  rebind to an attacker IP fails cert validation.

## 8. redirects  [THREAT: Info-disclosure via cross-host 3xx]

The proxy does NOT auto-follow redirects (the guest client does). But each new
request the client makes is a NEW CONNECT/request re-evaluated by policy from
scratch — so a 302 to evil.com results in the guest opening a tunnel to
evil.com, which has no policy ⇒ placeholder not substituted ⇒ no secret leaks.
Additionally: on any 3xx response whose Location points to a host != the
current allowlisted host, the proxy STRIPS the Authorization/custom header from
being re-sent and the redacted secret cannot appear in Location (redactor scans
Location header too, §5). Criterion-3 "redirect to other host strips secret"
covered by both: (a) secret never re-attached on a cross-host hop, (b) secret
value redacted out of any Location/body.

## 9. IP-literal CONNECT  [THREAT: policy bypass]

CONNECT to an IP literal (`1.2.3.4:443` or `[::1]:443`) has no hostname to
match a policy host ⇒ NEVER substituted (deny-by-default), tunnelled as
passthrough at most, and link-local/loopback/cs_host IPs are dropped entirely
(reuse fcnet.go fcClassifyDst / ctl-drop rules). Prevents using a raw IP to
dodge the exact-hostname policy or to hit host-local services.

## 10. pinned clients / passthrough  [THREAT: breakage, not security]

Hosts with NO policy are never MITM'd: cred-proxy does a plain TCP CONNECT
tunnel (bytes copied, no TLS termination), so certificate-pinning clients work
unchanged. MITM happens ONLY for hosts that have a policy (and those upstreams
are, by construction, ones we intend to inject into — the operator accepts MITM
for them). If a pinned client needs an allowlisted host, that is an explicit
operator decision recorded in policy.

## 11. interplay with existing proxy + network profiles

- `network=none`: guest has no NIC; today egress is the L7 vsock proxy
  (proxy.go, LLM only). cred-proxy adds a second L7 listener for credentialed
  HTTPS to allowlisted hosts. Both are host-terminated; no raw egress.
- `network=wan|lan|full`: guest has an L3 NIC (fcnet.go). For cred-proxy to be
  effective here, guests must still be pointed at cred-proxy via HTTPS_PROXY
  for the allowlisted hosts, OR the allowlisted hosts are force-routed through
  it. Direct L3 egress to an allowlisted host WITHOUT the proxy would mean the
  guest has no real secret anyway (only a placeholder), so the worst case is a
  failed auth, not a leak. Documented: cred-proxy provides the secret; without
  it the guest simply cannot authenticate.
- cs_host / loopback / daemon ports remain always-dropped for guests
  (fcnet.go), unchanged.

## 12. dependencies

Implementation uses stdlib ONLY: crypto/tls, crypto/x509, crypto/ecdsa,
crypto/rand, net, net/http, encoding/json, bufio, bytes, io, sync. NO new
third-party module. go.mod diff expected empty (verified in criterion 4). If
any helper is ever needed it must be pinned and ≥6 weeks old with justification
here — none is anticipated.

## 13. STRIDE summary

- Spoofing: SNI==CONNECT==Host enforced (§7); upstream verified vs system roots.
- Tampering: policy + secrets 0600 host-only; audit log append-only intent.
- Repudiation: audit log per decision (no secret values).
- Information disclosure: secret host-only; redactor on all responses/headers/
  Location/logs (§5); deny-by-default substitution (§4); body/query OFF (§6).
- Denial of service: leaf minting cached; streaming redactor bounded carry
  buffer; passthrough for un-policied hosts avoids parsing arbitrary TLS.
- Elevation of privilege: CA key never in guest; name constraints (§2);
  per-group allowlist; IP-literal + loopback blocked (§9).

## 14. test plan (criteria 3 & 5)

Unit (daemon/credproxy, `-run Cred`):
- placeholder present but host NOT allowlisted ⇒ no substitution + block.
- allowlisted host ⇒ substitution happens; upstream sees real secret.
- 3xx redirect to other host ⇒ secret not re-attached and redacted from Location.
- real secret appearing in a response body/header ⇒ redacted before guest sees it
  (incl. split across chunk boundaries).
- SNI/Host != CONNECT authority ⇒ block.
- IP-literal CONNECT ⇒ never substituted.
- policy matching: group/host/header exactness, deny-by-default.

E2E (bash, criterion 5): boot a guest, run a local test HTTPS server (minted
leaf) as the "upstream"; guest curls it with the placeholder; assert (a) the
test server's captured request carried the REAL secret, and (b) grepping the
guest fs/env/proc for the secret value returns 0 hits.
