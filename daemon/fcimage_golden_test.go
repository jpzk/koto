package main

import "testing"

// The default (no image=) VM config, captured byte-for-byte from develop
// before image support existed. Groups without image= must boot exactly this.
const (
	fcGoldenDefault0 = `{"boot-source":{"boot_args":"console=ttyS0 reboot=k panic=1 pci=off quiet init=/usr/local/bin/fc-agent","kernel_image_path":"/a/vmlinux"},"cpu-config":{"cpuid_modifiers":[{"flags":0,"leaf":"0x1","modifiers":[{"bitmap":"0bxxxxxxxxxxxxxxxxxxxxxxxxxx0xxxxx","register":"ecx"}],"subleaf":"0x0"},{"flags":0,"leaf":"0x80000001","modifiers":[{"bitmap":"0bxxxxxxxxxxxxxxxxxxxxxxxxxxxxx0xx","register":"ecx"}],"subleaf":"0x0"}]},"drives":[{"drive_id":"rootfs","is_read_only":true,"is_root_device":true,"path_on_host":"/a/rootfs.img","rate_limiter":{"bandwidth":{"one_time_burst":268435456,"refill_time":1000,"size":52428800},"ops":{"refill_time":1000,"size":3000}}},{"drive_id":"workspace","is_read_only":false,"is_root_device":false,"path_on_host":"/a/workspace.img","rate_limiter":{"bandwidth":{"one_time_burst":268435456,"refill_time":1000,"size":52428800},"ops":{"refill_time":1000,"size":3000}}}],"machine-config":{"mem_size_mib":1024,"smt":false,"vcpu_count":2},"vsock":{"guest_cid":3,"uds_path":"/vsock/v"}}`
	fcGoldenDefault1 = `{"boot-source":{"boot_args":"console=ttyS0 reboot=k panic=1 pci=off quiet init=/usr/local/bin/fc-agent","kernel_image_path":"/a/vmlinux"},"drives":[{"drive_id":"rootfs","is_read_only":true,"is_root_device":true,"path_on_host":"/a/rootfs.img","rate_limiter":{"bandwidth":{"one_time_burst":268435456,"refill_time":1000,"size":104857600},"ops":{"refill_time":1000,"size":6000}}},{"drive_id":"workspace","is_read_only":false,"is_root_device":false,"path_on_host":"/a/workspace.img","rate_limiter":{"bandwidth":{"one_time_burst":268435456,"refill_time":1000,"size":104857600},"ops":{"refill_time":1000,"size":6000}}}],"machine-config":{"mem_size_mib":4096,"smt":false,"vcpu_count":4},"vsock":{"guest_cid":3,"uds_path":"/vsock/v"}}`
)

func TestFcVMConfigDefaultUnchanged(t *testing.T) {
	got0 := string(fcVMConfig("g", "/a/vmlinux", "/a/rootfs.img", "/a/workspace.img", "/vsock/v", 2, 1024, 50<<20, 3000, false))
	got1 := string(fcVMConfig("g", "/a/vmlinux", "/a/rootfs.img", "/a/workspace.img", "/vsock/v", 4, 4096, 100<<20, 6000, true))
	if got0 != fcGoldenDefault0 {
		t.Fatalf("default config (kvm=no) changed:\n got %s\nwant %s", got0, fcGoldenDefault0)
	}
	if got1 != fcGoldenDefault1 {
		t.Fatalf("default config (kvm=yes) changed:\n got %s\nwant %s", got1, fcGoldenDefault1)
	}
	b, err := fcVMConfigFor("g", "/a/vmlinux", "/a/rootfs.img", "/a/workspace.img", "/vsock/v", 2, 1024, 50<<20, 3000, false, nil)
	if err != nil || string(b) != fcGoldenDefault0 {
		t.Fatalf("fcVMConfigFor(nil plan) != golden: err=%v\n got %s", err, b)
	}
}
