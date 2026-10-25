package main

// nerdfont.go — Nerd Font icons for the tree's session rows.
//
// Session rows show nf-oct-git_branch (U+F418) and goal-run rows
// (goal-<name>, goal-<name>-judge) nf-fa-bullseye (U+F140). Both live in the
// Private Use Area, so a terminal without a patched font draws tofu:
// KOTO_TUI_NERDFONT=0 restores the plain "◦ " marker. Resolved once at
// startup, like the mono flag — the render path reads it unsynchronized.
//
// The glyph constants and sessionMarker sit next to renderTreeRow in view.go,
// always as \u escapes: literal PUA bytes in source get stripped
// by tooling and transports that treat them as junk.

// nerdFont is the icon flag. Defaults on; written once by initNerdFont.
var nerdFont = true

func initNerdFont(env func(string) string) {
	nerdFont = resolveNerdFont(env)
}

// resolveNerdFont: only an explicit off value opts out; unset or anything
// else keeps the icons.
func resolveNerdFont(env func(string) string) bool {
	switch env("KOTO_TUI_NERDFONT") {
	case "0", "false", "no", "off":
		return false
	}
	return true
}
