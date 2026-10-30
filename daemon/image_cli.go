package main

// `koto image <op> ...` client: a thin streaming wrapper over the admin-only
// Image rpc (imagecmd.go does the work in the daemon).

import (
	"context"
	"fmt"
	"io"
	"os"

	"koto-protocol/pb"
)

const imageUsage = `usage: koto image <op> [args]
  add <name> <ref>            pull <ref> (tag or @digest), pin its digest under <name>
  ls                          list catalog names, digests, users
  inspect <name>              show one image (layers, created, libc, groups)
  rm <name>                   remove a name (refused while a group uses it)
  gc                          delete blobs/layers no name or group needs
  update <name> [<ref>]       re-resolve <name> and move it; groups stay put
  apply <name> <group..>|--all
                              move groups to <name>'s digest (fresh sys.img,
                              /workspace untouched, provision.sh once)
  rollback <group>            back to the previous image and its sys.img
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
	stream, err := ctlClient().Image(context.Background(), req)
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
		case "end":
			return
		case "error":
			ctlFatal(1, "%s", ev.Error)
		}
	}
}
