# Security policy

koto is a sandbox: its job is to keep untrusted agent workloads away from the
host, from each other, and from credentials. A flaw in that isolation is a
security issue, and we would much rather hear about it privately first.

## Reporting a vulnerability

Please use responsible disclosure: send the details to
**jendrik@madewithtea.com** instead of opening a public issue or pull request.

A useful report includes:

- what is affected (component, version or commit, host distribution)
- what an attacker can do, and from which position — a prompt-injected agent
  inside a group, another local user, a network peer
- steps to reproduce, or a proof of concept
- any suggested fix or mitigation

Please give us a reasonable chance to fix the issue before disclosing it
publicly. We will acknowledge your report, keep you updated while we work on
it, and credit you in the fix unless you ask us not to.

## Supported versions

Security fixes go into the latest release and `main`.

## Scope

The trust boundaries koto is meant to enforce are described in the
architecture section of the [README](README.md#architecture). Particularly
relevant:

- escaping a group's microVM or the jailed Firecracker process
- reaching the host, another group, or the network beyond a group's
  `network` profile
- reading or exfiltrating credentials held by the proxy
- bypassing the gRPC authentication, the role ACL, or the in-guest control
  plane's per-group authorization
- tampering with release artifacts or getting past their signature checks
