package main

import "testing"

func TestRootOverlayDirName(t *testing.T) {
	for in, want := range map[string]string{
		"":       ".rootovl",
		"fedora": ".rootovl",
		"kali":   ".rootovl-kali",
		"debian": ".rootovl-debian",
	} {
		if got := rootOverlayDirName(in); got != want {
			t.Errorf("rootOverlayDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOSIDPattern(t *testing.T) {
	for _, ok := range []string{"fedora", "kali", "opensuse-tumbleweed", "rhel"} {
		if !osIDRE.MatchString(ok) {
			t.Errorf("%q rejected", ok)
		}
	}
	// Anything that could escape the workspace dir when joined as a path.
	for _, bad := range []string{"", "../x", "a/b", ".hidden", "-x", "Kali"} {
		if osIDRE.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
}
