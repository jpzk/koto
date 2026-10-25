package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 1.7.2 fix(6), audit supply/sha256sums-not-version-bound (+ the
// supply/gpg-validsig-revoked-key and supply/fallback-clone-binaries
// candidates, and supply/make-dist-stale-asset-glob): install.sh — which
// `koto update` runs verbatim — must accept only the signed asset set OF THE
// REQUESTED VERSION, all five assets, from a key that is neither expired nor
// revoked.
//
// The script runs for real (`--download-only` against a file:// mirror). gpg,
// curl, zstd and id are stubs on PATH: gpg replays the status lines and exit
// code a scenario dictates, so what is under test is install.sh's decision
// logic, not gpg's cryptography.

const sec172FPR = "0123456789ABCDEF0123456789ABCDEF01234567"

var sec172Stubs = map[string]string{
	"gpg": `#!/bin/sh
for a in "$@"; do
	case "$a" in
	--import) cat >/dev/null 2>&1 </dev/null; exit 0 ;;
	--list-keys) echo "fpr:::::::::$STUB_FPR:"; exit 0 ;;
	--verify) cat "$STUB_GPG_STATUS"; exit "$STUB_GPG_RC" ;;
	esac
done
exit 0
`,
	"curl": `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift 2 ;;
	file://*) url="$1"; shift ;;
	*) shift ;;
	esac
done
[ -n "$url" ] && [ -n "$out" ] || exit 22
cp "${url#file://}" "$out" 2>/dev/null || exit 37
`,
	"zstd": `#!/bin/sh
in=""; out=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out="$2"; shift 2 ;;
	-*) shift ;;
	*) in="$1"; shift ;;
	esac
done
cp "$in" "$out"
`,
	"id": "#!/bin/sh\necho 1000\n",
}

type sec172Release struct {
	versionLine string   // "" = none (every pre-1.7.2 release)
	assets      []string // asset names, without the _x86_64.zst tail
	gpgStatus   string
	gpgRC       int
}

func sec172Good() string {
	return "[GNUPG:] NEWSIG\n" +
		"[GNUPG:] GOODSIG 89ABCDEF01234567 koto release\n" +
		"[GNUPG:] VALIDSIG " + sec172FPR + " 2026-10-01 1790000000 0 4 0 22 10 00 " + sec172FPR + "\n"
}

func sec172AllAssets(ver string) []string {
	return []string{"koto_" + ver, "koto-tui_" + ver, "firecracker_1.17.0", "vmlinux_6.1.186", "rootfs.img_fedora44"}
}

// runInstallFetch serves rel from a file:// mirror and runs
// `install.sh --download-only --version want`. It returns the exit error and
// the combined output.
func runInstallFetch(t *testing.T, rel sec172Release, want string, extraEnv ...string) (error, string, string) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Skip("install.sh is Linux/x86_64 only")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for _, tool := range []string{"sha256sum", "awk", "grep", "mktemp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	mirror := filepath.Join(root, "mirror")
	dest := filepath.Join(root, "dest")
	for _, d := range []string{bin, mirror, dest} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range sec172Stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var sums bytes.Buffer
	if rel.versionLine != "" {
		sums.WriteString(rel.versionLine + "\n")
	}
	for _, a := range rel.assets {
		file := a + "_x86_64.zst"
		body := []byte("asset " + a + "\n")
		if err := os.WriteFile(filepath.Join(mirror, file), body, 0o644); err != nil {
			t.Fatal(err)
		}
		h := sha256.Sum256(body)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(h[:]), file)
	}
	must := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(mirror, "SHA256SUMS"), sums.Bytes())
	must(filepath.Join(mirror, "SHA256SUMS.asc"), []byte("sig\n"))
	must(filepath.Join(root, "key.asc"), []byte("key\n"))
	must(filepath.Join(root, "status"), []byte(rel.gpgStatus))

	args := []string{"../install.sh", "--download-only", "--dest", dest}
	if want != "" {
		args = append(args, "--version", want)
	}
	cmd := exec.Command("sh", args...)
	cmd.Env = append([]string{
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"HOME=" + root,
		"KOTO_BASE_URL=file://" + mirror,
		"KOTO_RELEASE_PUBKEY=" + filepath.Join(root, "key.asc"),
		"KOTO_KEY_FPR=" + sec172FPR,
		"STUB_FPR=" + sec172FPR,
		"STUB_GPG_STATUS=" + filepath.Join(root, "status"),
		fmt.Sprintf("STUB_GPG_RC=%d", rel.gpgRC),
	}, extraEnv...)
	out, err := cmd.CombinedOutput()
	return err, string(out), dest
}

func TestSec172ReleaseAcceptsBoundSet(t *testing.T) {
	err, out, dest := runInstallFetch(t, sec172Release{
		versionLine: "# koto-version: 1.7.2",
		assets:      sec172AllAssets("1.7.2"),
		gpgStatus:   sec172Good(),
	}, "1.7.2")
	if err != nil {
		t.Fatalf("a correctly bound, complete, well-signed 1.7.2 set was refused: %v\n%s", err, out)
	}
	for _, rel := range []string{"koto", "koto-tui", "fcassets/firecracker", "fcassets/vmlinux", "fcassets/rootfs.img"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("%s not unpacked: %v\n%s", rel, err, out)
		}
	}
	// A leading v on the requested tag is the same release.
	if err, out, _ := runInstallFetch(t, sec172Release{
		versionLine: "# koto-version: 1.7.2",
		assets:      sec172AllAssets("1.7.2"),
		gpgStatus:   sec172Good(),
	}, "v1.7.2"); err != nil {
		t.Fatalf("--version v1.7.2 refused a 1.7.2 set: %v\n%s", err, out)
	}
}

func TestSec172ReleaseRefusesUnboundOrIncompleteSet(t *testing.T) {
	cases := []struct {
		name string
		rel  sec172Release
		want string // the requested version
		msg  string // the refusal must say why
	}{
		{"older signed set replayed under a newer tag (no version line)",
			sec172Release{"", sec172AllAssets("1.7.1"), sec172Good(), 0}, "1.7.2", "koto-version"},
		{"signed for another version",
			sec172Release{"# koto-version: 1.7.1", sec172AllAssets("1.7.1"), sec172Good(), 0}, "1.7.2", "not the 1.7.2 you asked for"},
		{"two version lines",
			sec172Release{"# koto-version: 1.7.2\n# koto-version: 1.7.1", sec172AllAssets("1.7.2"), sec172Good(), 0}, "1.7.2", "exactly one koto-version"},
		{"koto asset from another release",
			sec172Release{"# koto-version: 1.7.2", []string{"koto_1.7.1", "koto-tui_1.7.2", "firecracker_1.17.0", "vmlinux_6.1.186", "rootfs.img_fedora44"}, sec172Good(), 0}, "1.7.2", "not koto 1.7.2"},
		{"koto-tui missing (clone binary fallback)",
			sec172Release{"# koto-version: 1.7.2", []string{"koto_1.7.2", "firecracker_1.17.0", "vmlinux_6.1.186", "rootfs.img_fedora44"}, sec172Good(), 0}, "1.7.2", "has no koto-tui asset"},
		{"koto missing",
			sec172Release{"# koto-version: 1.7.2", []string{"koto-tui_1.7.2", "firecracker_1.17.0", "vmlinux_6.1.186", "rootfs.img_fedora44"}, sec172Good(), 0}, "1.7.2", "has no koto asset"},
		{"rootfs listed twice",
			sec172Release{"# koto-version: 1.7.2", append(sec172AllAssets("1.7.2"), "rootfs.img_fedora43"), sec172Good(), 0}, "1.7.2", "twice"},
		{"expired key (gpg exit 0, VALIDSIG present)",
			sec172Release{"# koto-version: 1.7.2", sec172AllAssets("1.7.2"),
				"[GNUPG:] NEWSIG\n[GNUPG:] KEYEXPIRED 1780000000\n[GNUPG:] EXPKEYSIG 89ABCDEF01234567 koto release\n[GNUPG:] VALIDSIG " + sec172FPR + " 2026-10-01 1790000000 0 4 0 22 10 00 " + sec172FPR + "\n", 0},
			"1.7.2", "expired or revoked"},
		{"revoked key (gpg exit 0, VALIDSIG present)",
			sec172Release{"# koto-version: 1.7.2", sec172AllAssets("1.7.2"),
				"[GNUPG:] NEWSIG\n[GNUPG:] KEYREVOKED\n[GNUPG:] REVKEYSIG 89ABCDEF01234567 koto release\n[GNUPG:] VALIDSIG " + sec172FPR + " 2026-10-01 1790000000 0 4 0 22 10 00 " + sec172FPR + "\n", 0},
			"1.7.2", "expired or revoked"},
		{"gpg exit status non-zero despite VALIDSIG",
			sec172Release{"# koto-version: 1.7.2", sec172AllAssets("1.7.2"), sec172Good(), 2}, "1.7.2", "gpg rejected"},
		{"mirror without an explicit version",
			sec172Release{"# koto-version: 1.7.2", sec172AllAssets("1.7.2"), sec172Good(), 0}, "", "needs --version"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err, out, dest := runInstallFetch(t, c.rel, c.want)
			if err == nil {
				t.Fatalf("install.sh accepted it:\n%s", out)
			}
			if !strings.Contains(out, c.msg) {
				t.Errorf("refused, but not for the right reason (want %q):\n%s", c.msg, out)
			}
			if _, err := os.Stat(filepath.Join(dest, "koto")); err == nil {
				t.Errorf("koto was unpacked into the destination despite the refusal")
			}
		})
	}
}

// make dist must clear stale assets with the separator assets actually use,
// BEFORE packaging, and write the version line it signs.
func TestSec172MakeDistBindsVersion(t *testing.T) {
	b, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	mk := string(b)
	i := strings.Index(mk, "\ndist: ")
	if i < 0 {
		t.Fatal("no dist target")
	}
	recipe := mk[i+1:]
	if j := strings.Index(recipe, "\n\n"); j > 0 {
		recipe = recipe[:j]
	}
	if strings.Contains(recipe, "*-$(DIST_ARCH).zst") {
		t.Error("dist still clears *-$(DIST_ARCH).zst; assets are <name>_<ver>_<arch>.zst")
	}
	rm := strings.Index(recipe, "rm -f $(DIST_DIR)/*_$(DIST_ARCH).zst")
	pack := strings.Index(recipe, "zstd -$(DIST_ZSTD_LVL)")
	if rm < 0 || pack < 0 || rm > pack {
		t.Errorf("dist must rm -f $(DIST_DIR)/*_$(DIST_ARCH).zst before the packaging loop (rm at %d, zstd at %d)", rm, pack)
	}
	if !strings.Contains(recipe, `# koto-version: `) {
		t.Error("dist does not write a '# koto-version: X.Y.Z' line into SHA256SUMS")
	}
}
