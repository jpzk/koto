package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

// `koto audit verify [--dir D]`: walk the hash-chained spool offline (no
// daemon, no network) and report the first broken link. Exit 0 = intact,
// 1 = tampered/broken (or unreadable), 2 = usage.
func auditCliMain(args []string) {
	os.Exit(auditCli(args, os.Stdout, os.Stderr))
}

func auditCli(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "verify" {
		fmt.Fprintln(stderr, "usage: koto audit verify [--dir <spool dir>]")
		return 2
	}
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "", "spool directory (default <koto home>/audit)")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *dir == "" {
		*dir = auditDir(kotoHome())
	}
	res, err := auditVerify(*dir)
	if err != nil {
		var ve *auditVerifyError
		if errors.As(err, &ve) {
			fmt.Fprintf(stderr, "audit verify: TAMPERED: first broken link at %s\n", ve.Error())
		} else {
			fmt.Fprintf(stderr, "audit verify: %v\n", err)
		}
		fmt.Fprintf(stderr, "audit verify: %d records verified before the break\n", res.Records)
		return 1
	}
	anchor := "genesis"
	if res.Anchored {
		anchor = fmt.Sprintf("anchored at seq %d (older files rotated away)", res.FirstSeq)
	}
	fmt.Fprintf(stdout, "audit verify: OK %d records, seq %d..%d, %d file(s), %s, head %s\n",
		res.Records, res.FirstSeq, res.LastSeq, len(res.Files), anchor, res.LastHash)
	return 0
}
