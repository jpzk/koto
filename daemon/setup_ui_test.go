package main

// The line reader is what stands between the operator and a prompt that
// cannot be answered. A terminal left with ICRNL cleared delivers Enter as a
// bare \r, and the earlier ReadString('\n') blocked forever on it — the
// operator saw their keystrokes echo as ^M and nothing else. These pin the
// three line endings that reach us and the EOF abort.

import (
	"bufio"
	"strings"
	"testing"
)

func testUI(in string) *setupUI {
	// repaired: this is not a terminal, and the ioctl would no-op anyway —
	// but saying so keeps the test about the parsing.
	return &setupUI{in: bufio.NewReader(strings.NewReader(in)), repaired: true}
}

func TestReadLineAcceptsEveryLineEnding(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []string
	}{
		{"lf", "one\ntwo\n", []string{"one", "two"}},
		{"cr only (raw tty)", "one\rtwo\r", []string{"one", "two"}},
		{"crlf", "one\r\ntwo\r\n", []string{"one", "two"}},
		{"bare enter is the default", "\n", []string{""}},
		{"cr enter is the default too", "\r", []string{""}},
		{"unterminated final line", "one\nlast", []string{"one", "last"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := testUI(tc.in)
			for i, want := range tc.want {
				got, err := u.readLine()
				if err != nil {
					t.Fatalf("line %d: %v", i, err)
				}
				if got != want {
					t.Fatalf("line %d = %q, want %q", i, got, want)
				}
			}
		})
	}
}

func TestReadLineAbortsOnEOF(t *testing.T) {
	u := testUI("")
	if _, err := u.readLine(); err != errSetupAborted {
		t.Fatalf("err = %v, want errSetupAborted", err)
	}
}

// A CR-terminated menu answer must select, not loop. This is the exact
// interaction that hung: `choice` reads, gets nothing, and reprompts forever.
func TestChoiceAnswersOnCarriageReturn(t *testing.T) {
	u := testUI("2\r")
	if got := u.choice("pick", []string{"a", "b"}, 0); got != 1 {
		t.Fatalf("choice = %d, want 1", got)
	}
}

func TestYesNoAnswersOnCarriageReturn(t *testing.T) {
	if u := testUI("n\r"); u.yesno("really?", true) {
		t.Fatal("yesno = true, want false")
	}
}
