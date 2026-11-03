package ociimage

// The well-known base images: the ONE list of distro images koto offers by
// name, so a picker (the TUI new-group dialog, the setup wizard) never
// carries a registry reference of its own. Clients read it through the
// admin-only Image rpc, op "known" (daemon/imagecmd.go), and add an entry
// with `image add <name>` (no ref): the daemon fills the ref from here.
//
// Each ref but fedora's is a moving tag on purpose: "latest" means the tag is resolved
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
	Name  string // catalog name it is added under
	Label string // what a picker shows
	Ref   string // registry reference it is pulled from
}

// FedoraName is the catalog name of the default base image: every group
// with no image= (or an empty one) boots it, together with guest.img.
const FedoraName = "fedora"

// FedoraRef pins the default base by digest, unlike the other well-known
// names: it is the same fedora:44 digest fcguest/Dockerfile.rootfs and
// fcguest/Dockerfile.guest build FROM, so a group that had no image on the
// old built-in rootfs lands on the same Fedora userland. Groups with no
// image follow this pin across releases (a bump moves them, with a fresh
// sys.img); a group that wants a fixed base sets image= explicitly.
const FedoraRef = "docker.io/library/fedora@sha256:be9d65e2344d805cc11114319c685ecaa96b6d9b4350a0a6460cdb931babbd19"

// The built-in DefaultName (the legacy rootfs) is no longer offered by a
// picker: a picker's "no image" row now means fedora. It stays reachable
// by name (image=koto-default) as a one-release opt-out.
var knownImages = []KnownImage{
	{Name: FedoraName, Label: "fedora (default)", Ref: FedoraRef},
	{Name: "debian", Label: "debian", Ref: "docker.io/library/debian:latest"},
	{Name: "ubuntu", Label: "ubuntu", Ref: "docker.io/library/ubuntu:latest"},
	{Name: "kali", Label: "kali", Ref: "docker.io/kalilinux/kali-rolling:latest"},
}

// Known returns the well-known images in picker order, the default first.
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
