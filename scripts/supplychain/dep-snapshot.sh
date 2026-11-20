#!/usr/bin/env bash
# dep-snapshot.sh — submit the SBOM's Go / cargo / npm components to GitHub's
# dependency submission API, so Dependabot alerts (and dependency review)
# cover what koto SHIPS, not only what the repo's manifests declare:
# Firecracker's crates and guest.img's npm packages have no manifest in this
# repository, and the Go set is the set linked into each binary.
#
#   dep-snapshot.sh SBOM.cdx.json            build + POST the snapshot
#   dep-snapshot.sh --dry-run SBOM.cdx.json  print the snapshot JSON only
#
# Env (set by GitHub Actions): GITHUB_TOKEN (contents: write),
# GITHUB_REPOSITORY, GITHUB_API_URL, GITHUB_RUN_ID, GITHUB_WORKFLOW,
# GITHUB_JOB; SNAPSHOT_SHA / SNAPSHOT_REF = the commit the SBOM was built
# from and its fully-qualified ref (refs/heads/...).
#
# One manifest per shipped root — koto, koto-tui, firecracker, guest.img —
# each listing the root's transitive closure from the SBOM's dependency
# graph, "direct" for the root's own edges. Purl qualifiers are dropped
# (Dependabot matches on type/namespace/name@version); the Go standard
# library and koto's own modules are left out (not Dependabot-tracked
# packages). Exit 0 only on HTTP 201.
set -euo pipefail
dry=0
if [ "${1:-}" = --dry-run ]; then dry=1; shift; fi
sbom=${1:?usage: dep-snapshot.sh [--dry-run] SBOM.cdx.json}
command -v jq >/dev/null || { echo "dep-snapshot: jq required" >&2; exit 2; }
jq -e '.bomFormat == "CycloneDX"' "$sbom" >/dev/null || { echo "dep-snapshot: $sbom is not CycloneDX" >&2; exit 2; }

sha=${SNAPSHOT_SHA:-$(git rev-parse HEAD)}
ref=${SNAPSHOT_REF:-refs/heads/$(git rev-parse --abbrev-ref HEAD)}
[[ $sha =~ ^[0-9a-f]{40}$ ]] || { echo "dep-snapshot: bad sha '$sha'" >&2; exit 2; }
[[ $ref == refs/* ]] || { echo "dep-snapshot: ref '$ref' is not fully qualified" >&2; exit 2; }

snap=$(jq -c \
  --arg sha "$sha" --arg ref "$ref" \
  --arg corr "${GITHUB_WORKFLOW:-local}/sbom-snapshot" \
  --arg run "${GITHUB_RUN_ID:-0}" \
  --arg scanned "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
  def strip: sub("[?#].*$"; "");
  def tracked: test("^pkg:(golang|cargo|npm)/") and (test("^pkg:golang/std@") | not)
               and (test("^pkg:golang/(github\\.com/jpzk/)?koto([-/@]|$)") | not);
  (.components | map({key: .["bom-ref"], value: .}) | from_entries) as $c
  | (.dependencies | map({key: .ref, value: (.dependsOn // [])}) | from_entries) as $d
  | def closure($s): {seen: {}, todo: [$s]}
      | until(.todo | length == 0;
          .todo[0] as $x | .todo |= .[1:]
          | if .seen[$x] then . else .seen[$x] = true | .todo += ($d[$x] // []) end)
      | .seen | keys - [$s];
    def purl($r): ($c[$r].purl // "") | strip;
    # direct = tracked packages reachable from the root through untracked
    # nodes only (fc-agent inside guest.img, the pi install, std), i.e.
    # what the root pulls in itself rather than through another package.
    def directs($s): {seen: {}, todo: ($d[$s] // []), out: []}
      | until(.todo | length == 0;
          .todo[0] as $x | .todo |= .[1:]
          | if .seen[$x] then .
            elif (purl($x) | tracked) then .seen[$x] = true | .out += [$x]
            else .seen[$x] = true | .todo += ($d[$x] // []) end)
      | .out;
    def manifest($root; $name; $src):
      directs($root) as $direct
      | {key: $name, value: {name: $name, file: {source_location: $src},
          metadata: {sbom: "koto CycloneDX (scripts/supplychain/sbom.sh)"},
          resolved: ([closure($root)[] | select(purl(.) | tracked)
            | . as $r | {key: purl($r), value: {
                package_url: purl($r),
                relationship: (if ($direct | index($r)) then "direct" else "indirect" end),
                scope: (if $c[$r].scope == "excluded" then "development" else "runtime" end),
                dependencies: ([($d[$r] // [])[] | purl(.) | select(tracked)] | unique)}}]
            | from_entries)}};
    def rootref(pred): [.components[] | select(pred) | .["bom-ref"]] | if length == 1 then .[0] else error("root not unique") end;
  {version: 0, sha: $sha, ref: $ref,
   job: {correlator: $corr, id: $run},
   detector: {name: "koto-sbom", version: "1", url: "https://github.com/jpzk/koto/tree/develop/scripts/supplychain"},
   scanned: $scanned,
   manifests: ([
     manifest(rootref(any(.properties[]?; .name == "koto:binary" and .value == "koto")); "koto"; "daemon/go.mod"),
     manifest(rootref(any(.properties[]?; .name == "koto:binary" and .value == "koto-tui")); "koto-tui"; "tui/go.mod"),
     manifest(rootref(.name == "firecracker" and .type == "application"); "firecracker"; "fcguest/build-firecracker.sh"),
     manifest(rootref(.name == "guest.img"); "guest.img"; "fcguest/Dockerfile.guest")
   ] | from_entries)}' "$sbom")

echo "$snap" | jq -r '.manifests[] | "dep-snapshot: manifest \(.name): \(.resolved | length) packages (\([.resolved[] | select(.relationship == "direct")] | length) direct)"'
for m in koto koto-tui firecracker guest.img; do
  n=$(echo "$snap" | jq --arg m "$m" '.manifests[$m].resolved | length')
  [ "$n" -gt 0 ] || { echo "dep-snapshot: manifest $m is empty" >&2; exit 1; }
done
if [ $dry = 1 ]; then echo "$snap" | jq .; exit 0; fi

: "${GITHUB_TOKEN:?dep-snapshot: GITHUB_TOKEN unset}"
: "${GITHUB_REPOSITORY:?dep-snapshot: GITHUB_REPOSITORY unset}"
api=${GITHUB_API_URL:-https://api.github.com}
resp=$(mktemp)
code=$(printf '%s' "$snap" | curl -sS -o "$resp" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $GITHUB_TOKEN" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  --data-binary @- "$api/repos/$GITHUB_REPOSITORY/dependency-graph/snapshots")
echo "dep-snapshot: POST /repos/$GITHUB_REPOSITORY/dependency-graph/snapshots sha=$sha ref=$ref -> HTTP $code"
jq -c '{id, result, message, created_at}' "$resp" 2>/dev/null || head -c 500 "$resp"; echo
rm -f "$resp"
[ "$code" = 201 ]
