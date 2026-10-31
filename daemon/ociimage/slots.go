package ociimage

import (
	"fmt"
	"strings"
)

// Firecracker x86 device budget, measured in the PoC (goal/oci-layers
// DESIGN.md 2.1): 17 virtio-blk drives boot; the 18th fails at config time
// with AttachAcpiDevice(...AllocateGsi(ResourceNotAvailable)) because every
// virtio-mmio device takes a legacy GSI and the ACPI devices attached after
// them get none.
const FCMaxMMIODevices = 17

// FixedDevices is the device list every image-mode VM carries besides its
// layers, in drive order. The daemon's fcVMConfig builds the image-mode drive
// list FROM this slice, so adding a device here (or there) shrinks the layer
// budget automatically.
//
// "reserve" is one slot of headroom: the combination 16 drives + vsock was
// not measured on its own (the PoC measured 17 drives without vsock and
// 14 layers + 3 drives + net + vsock budgeting), and a future virtio-net or
// balloon device must not silently push working images over the cap.
var FixedDevices = []string{"tools.img", "workspace.img", "sys.img", "vsock", "reserve"}

// LayerSlots is how many layer drives an image may use.
func LayerSlots() int { return FCMaxMMIODevices - len(FixedDevices) }

// CheckLayerSlots refuses images with more layers than the VM can attach.
// No squashing: images stay below the limit (operator decision 2026-10-30).
func CheckLayerSlots(n int) error {
	if s := LayerSlots(); n > s {
		return fmt.Errorf("image has %d layers but a koto VM can attach at most %d "+
			"(firecracker allows %d devices; koto uses %d: %s); rebuild the image with fewer layers",
			n, s, FCMaxMMIODevices, len(FixedDevices), strings.Join(FixedDevices, ", "))
	}
	return nil
}
