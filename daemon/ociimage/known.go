package ociimage

// The well-known base images: the ONE list of distro images koto offers by
// name, so a picker (the TUI new-group dialog, the setup wizard) never
// carries a registry reference of its own. Clients read it through the
// admin-only Image rpc, op "known" (daemon/imagecmd.go), and add an entry
// with `image add <name>` (no ref): the daemon fills the ref from here.
//
// Each ref is a moving tag on purpose: "latest" means the tag is resolved
// to a digest once, at add (or update) time, and that digest is pinned in
// the catalog like any other add. koto has no image age gate (operator
// decision); the created date is shown, never enforced.
//
// debian is debian:latest rather than debian:stable-slim: it is the same
// release (latest == current stable), the one ubuntu and kali are also
// offered as (their full, non-slim tag), and it keeps man pages and docs
// an agent working in the VM can read; slim saves ~20MB compressed.

// KnownImage is one well-known base image.
type KnownImage struct {
	Name  string // catalog name it is added under (DefaultName = built-in)
	Label string // what a picker shows
	Ref   string // registry reference it is pulled from; "" for the built-in
}

var knownImages = []KnownImage{
	{Name: DefaultName, Label: "default"},
	{Name: "debian", Label: "debian", Ref: "docker.io/library/debian:latest"},
	{Name: "ubuntu", Label: "ubuntu", Ref: "docker.io/library/ubuntu:latest"},
	{Name: "kali", Label: "kali", Ref: "docker.io/kalilinux/kali-rolling:latest"},
}

// Known returns the well-known images in picker order, the built-in first.
// The slice is a copy.
func Known() []KnownImage {
	return append([]KnownImage(nil), knownImages...)
}

// KnownRef returns the registry reference a well-known name is pulled from.
// ok is false for an unknown name and for the built-in (nothing to pull).
func KnownRef(name string) (ref string, ok bool) {
	for _, k := range knownImages {
		if k.Name == name && k.Ref != "" {
			return k.Ref, true
		}
	}
	return "", false
}
