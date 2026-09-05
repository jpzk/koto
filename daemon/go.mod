module koto

go 1.25.0

toolchain go1.25.14

require (
	github.com/containers/gvisor-tap-vsock v0.8.8
	golang.org/x/sys v0.47.0
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.11
	koto-protocol v0.0.0
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/apparentlymart/go-cidr v1.1.0 // indirect
	github.com/google/btree v1.1.2 // indirect
	github.com/google/gopacket v1.1.19 // indirect
	github.com/inetaf/tcpproxy v0.0.0-20250222171855-c4b9df066048 // indirect
	github.com/insomniacslk/dhcp v0.0.0-20240710054256-ddd8a41251c9 // indirect
	github.com/miekg/dns v1.1.72 // indirect
	github.com/pierrec/lz4/v4 v4.1.14 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/mod v0.37.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.5.0 // indirect
	golang.org/x/tools v0.47.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260414002931-afd174a4e478 // indirect
	gvisor.dev/gvisor v0.0.0-20240916094835-a174eb65023f // indirect
)

replace koto-protocol => ../protocol
