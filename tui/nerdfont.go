package main

// nerdfont.go — Nerd Font icons for the TUI, and the one table they live in.
//
// Every pictograph the TUI draws (tree dots, transcript glyphs, the
// notification bell, goal/sched lifecycle lines) is an icon below: a Nerd
// Font glyph plus the plain Unicode it replaces. ic() picks one by the
// nerdFont flag. All NF glyphs sit in the Private Use Area, so a terminal
// without a patched font draws tofu: KOTO_TUI_NERDFONT=0 restores the plain
// glyphs byte for byte (TestNerdSitesOffGolden pins that). Resolved once at
// startup, like the mono flag — the render path reads it unsynchronized.
//
// Rules for this table:
//   - glyphs are \u escapes, never literals: PUA bytes get stripped by
//     tooling and transports that treat them as junk.
//   - an icon's nf form is the same cell width as its plain form (emoji are
//     two cells, NF glyphs one, so those NF forms carry a trailing space).
//     TestNerdIconWidths enforces it: the swap must never move a column.
//   - every NF codepoint here needs a monoGlyphs entry (mono.go);
//     TestNerdIconsMonoFallback scans this file and enforces it.

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

// icon is one pictograph: nf when Nerd Font is on, plain otherwise.
type icon struct{ nf, plain string }

// ic renders i for the current font mode.
func ic(i icon) string {
	if nerdFont {
		return i.nf
	}
	return i.plain
}

var (
	// tree + transcript state markers (1 cell)
	icWarn  = icon{"", "⚠"} // nf-fa-warning — stalled group, orphaned job
	icCog   = icon{"", "⚙"} // nf-fa-cog — running job, tool call
	icCheck = icon{"", "✓"} // nf-fa-check — job ok, enabled sched, ok marks
	icCross = icon{"", "✗"} // nf-fa-times — job failed, errors, goal cancelled
	icPlay  = icon{"", "▶"} // nf-fa-play — goal resumed, sched run
	icPause = icon{"", "⏸"} // nf-fa-pause — goal paused
	icScale = icon{"", "⚖"} // nf-fa-balance_scale — goal judge/verdict
	icGoal  = icon{"", "◎"} // nf-fa-bullseye — goal lifecycle

	// wide (two-cell) emoji: NF glyph + one space keeps the width
	icBell      = icon{" ", "🔔"} // nf-fa-bell — notifications
	icThought   = icon{" ", "🧠"} // nf-fa-lightbulb_o — thinking
	icToolOut   = icon{" ", "📤"} // nf-fa-upload — tool output
	icHourglass = icon{" ", "⏳"} // nf-fa-hourglass_half — queued
	icAlarm     = icon{" ", "⏰"} // nf-fa-clock_o — sched fired
	icMet       = icon{" ", "✅"} // nf-fa-check_circle — goal met

	// tree session markers, glyph + trailing space (see sessionMarker)
	icSession     = icon{" ", "◦ "} // nf-oct-git_branch — chat session row
	icGoalSession = icon{" ", "◦ "} // nf-fa-bullseye — goal-run session row
)

// allIcons lists every icon above, for the width / fallback tests.
var allIcons = []icon{
	icWarn, icCog, icCheck, icCross, icPlay, icPause, icScale, icGoal,
	icBell, icThought, icToolOut, icHourglass, icAlarm, icMet,
	icSession, icGoalSession,
}
