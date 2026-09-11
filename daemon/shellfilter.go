package main

// shellfilter.go — the containment for `koto ctl shell`.
//
// The TUI renders guest pty bytes through a terminal EMULATOR: they become a
// screen grid and only safe cell content reaches the operator's terminal (see
// tui/shell_view.go and vt_scrub.go). The CLI attach has no emulator — it puts
// the terminal in raw mode and relays bytes straight to os.Stdout, because
// that is what makes vim, tmux and colours work — so until now a process in
// the guest could drive the operator's terminal directly (audit M86).
//
// Blanket sanitization is the wrong fix here: strip CSI and the attach stops
// being a terminal. What this does instead is remove the sequence classes that
// are never needed to DRAW and are the ones that do harm:
//
//   - OSC (ESC ]) — clipboard writes (OSC 52), window/icon title, hyperlinks.
//   - DCS/APC/PM/SOS (ESC P / ESC _ / ESC ^ / ESC X) — device control strings.
//   - CSI QUERIES: Device Status Report (...n) and Device Attributes (...c).
//     These make the operator's terminal WRITE A REPLY back into the pty, i.e.
//     inject attacker-chosen bytes into the guest's stdin.
//
// Everything else passes: text, C0, and CSI for cursor movement, modes,
// scrolling regions and SGR. So an editor still works and a hostile guest
// cannot reach past the screen.
//
// Stateful, because a chunk boundary can fall anywhere inside a sequence — the
// filter is fed whatever the vsock relay happens to deliver.

type shellFilter struct {
	state shellFilterState
	// csi accumulates a CSI sequence's parameter/intermediate bytes so the
	// final byte can be judged against the whole thing. Bounded: a sequence
	// longer than this is malformed and is dropped wholesale.
	csi []byte
	// escSeen is set inside a string sequence when an ESC arrives, so the
	// ST terminator (ESC \) is recognized across it.
	escSeen bool
}

type shellFilterState int

const (
	sfText    shellFilterState = iota // ordinary output
	sfEsc                             // just saw ESC
	sfCSI                             // inside ESC [ ...
	sfString                          // inside OSC/DCS/APC/PM/SOS, dropping to ST/BEL
	sfCSIDrop                         // inside a CSI being dropped (a query)
)

// shellFilterMaxCSI bounds the buffered CSI sequence.
const shellFilterMaxCSI = 64

// filter returns the bytes safe to write to the operator's terminal.
func (f *shellFilter) filter(in []byte) []byte {
	out := make([]byte, 0, len(in))
	for _, b := range in {
		switch f.state {
		case sfText:
			if b == 0x1b {
				f.state = sfEsc
				continue
			}
			// C1 forms of the same introducers, which a UTF-8-decoding
			// terminal can still act on: 0x9b CSI, 0x9d OSC, 0x90 DCS,
			// 0x9e PM, 0x9f APC, 0x98 SOS.
			switch b {
			case 0x9b:
				f.state, f.csi = sfCSI, f.csi[:0]
				continue
			case 0x90, 0x98, 0x9d, 0x9e, 0x9f:
				f.state, f.escSeen = sfString, false
				continue
			}
			out = append(out, b)

		case sfEsc:
			switch b {
			case '[':
				f.state, f.csi = sfCSI, f.csi[:0]
			case ']', 'P', '_', '^', 'X':
				f.state, f.escSeen = sfString, false
			case 0x1b:
				// ESC ESC — stay here rather than emitting a stray ESC.
			default:
				// A two-byte escape (ESC =, ESC >, ESC M, charset selects…).
				// These are drawing/mode controls; pass them through.
				out = append(out, 0x1b, b)
				f.state = sfText
			}

		case sfCSI, sfCSIDrop:
			if len(f.csi) < shellFilterMaxCSI {
				f.csi = append(f.csi, b)
			} else {
				f.state = sfCSIDrop
			}
			// A CSI ends at a byte in 0x40..0x7e.
			if b >= 0x40 && b <= 0x7e {
				drop := f.state == sfCSIDrop || b == 'n' || b == 'c'
				if !drop {
					out = append(out, 0x1b, '[')
					out = append(out, f.csi...)
				}
				f.state, f.csi = sfText, f.csi[:0]
			}

		case sfString:
			// Terminated by BEL, or by ST (ESC \ / 0x9c).
			switch {
			case b == 0x07 || b == 0x9c:
				f.state = sfText
			case f.escSeen && b == '\\':
				f.state = sfText
			}
			f.escSeen = b == 0x1b
		}
	}
	return out
}
