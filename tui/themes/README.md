# themes/

Color palettes for the TUI, in the [Hundred Rabbits theme format](https://github.com/hundredrabbits/Themes).

Every `.svg` here is one palette: a swatch sheet, not a drawing. A
`<rect id="background">` plus eight `<circle>` elements whose `id` is the role
and whose `fill` is the color — three foreground tiers, three background tiers,
and an inverse pair. It renders as a preview of itself in any browser, which is
why the format was chosen over a config file.

Parsed by `../theme.go` (`parseTheme`), embedded into the binary with
`go:embed themes/*.svg`, and selected at runtime with `/themes`.

## Adding one

Drop the `.svg` in and rebuild (`make tui-build`). `TestBundledThemesParse`
fails if it doesn't carry all nine roles; `TestEveryThemeIsLegible` fails if the
contrast repair can't make it readable in this TUI.

To add a palette **without** rebuilding, put it in `run/tui/themes/` on the host
(`/koto-run/themes` inside `cs_tui`) — that directory is scanned at startup and
a file there shadows a bundled one of the same name.

## Provenance

The bundled set is the upstream `hundredrabbits/Themes` collection, unmodified,
vendored under its MIT license (see `LICENSE` in this directory).

Copyright (c) Devine Lu Linvega.
