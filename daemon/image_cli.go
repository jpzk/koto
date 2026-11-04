package main

// `koto image <op> ...` client: a thin streaming wrapper over the admin-only
// Image rpc (imagecmd.go does the work in the daemon). Connects as the admin
// client `tui` unless KOTO_CLIENT is set (imageCliDefaultIdentity, ctl_cli.go).

import (
	"context"
	"fmt"
	"io"
	"os"

	"koto-protocol/pb"
)

const imageUsage = `usage: koto image <op> [args]
  add <name> [<ref>]          pull <ref> (tag or @digest), pin its digest under <name>;
                              <ref> may be omitted for debian, ubuntu, kali (latest)
  known                       the well-known images + catalog names (TSV, for pickers)
  ls                          list catalog names, digests, users
  inspect <name>              show one image (layers, created, libc, groups)
  rm <name>                   remove a name (refused while a group uses it)
  gc                          delete blobs/layers no name or group needs
  update <name> [<ref>]       re-resolve <name> and move it; groups stay put
  apply <name> <group..>|--all
                              move groups to <name>'s digest (fresh sys.img,
                              /workspace untouched, provision.sh once)
  rollback <group>            back to the previous image and its sys.img

KOTO_IMAGE_PROGRESS=1 also prints the structured add/update progress events
(phase, layer i/n, bytes done/total) on stderr, one line each: resolve,
download, verify/cached (blob), convert (tar bytes consumed building the
layer ext4), layer_cached (ext4 already on disk), done. The first boot of a
group logs its own progress phase=convert/sysimg lines in the daemon log.
`

// imageCliParse turns argv into an ImageReq. --all/-all is only for apply.
func imageCliParse(args []string) (*pb.ImageReq, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		return nil, fmt.Errorf("help")
	}
	r := &pb.ImageReq{Op: args[0]}
	for _, a := range args[1:] {
		if a == "--all" || a == "-all" {
			if r.Op != "apply" {
				return nil, fmt.Errorf("--all is only valid for apply")
			}
			r.All = true
			continue
		}
		r.Args = append(r.Args, a)
	}
	return r, nil
}

func imageCliMain(args []string) {
	req, err := imageCliParse(args)
	if err != nil {
		fmt.Fprint(os.Stderr, imageUsage)
		if err.Error() == "help" {
			os.Exit(0)
		}
		ctlFatal(2, "%v", err)
	}
	showProgress := os.Getenv("KOTO_IMAGE_PROGRESS") == "1"
	stream, err := ctlClientAs(imageCliDefaultIdentity).Image(context.Background(), req)
	if err != nil {
		ctlFatal(1, "image: %v", err)
	}
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			ctlFatal(1, "stream: %v", err)
		}
		switch ev.Event {
		case "data":
			fmt.Print(ev.Data)
		case "progress":
			if p := ev.GetProgress(); p != nil && showProgress {
				fmt.Fprintf(os.Stderr, "progress phase=%s image=%s layer=%d/%d done=%d total=%d\n",
					p.GetPhase(), p.GetImage(), p.GetLayer(), p.GetLayers(), p.GetDone(), p.GetTotal())
			}
		case "end":
			return
		case "error":
			ctlFatal(1, "%s", ev.Error)
		}
	}
}
