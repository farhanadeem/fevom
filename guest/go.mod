module fev/guest

go 1.27

require (
	fev/proto v0.0.0-00010101000000-000000000000
	github.com/cilium/ebpf v0.22.0
	github.com/flosch/pongo2/v6 v6.1.0
	github.com/klauspost/compress v1.20.1
	github.com/mdlayher/vsock v1.3.0
	github.com/pkg/sftp v1.13.11
	github.com/ulikunitz/xz v0.5.17
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/crypto/x509roots/fallback v0.0.0-20261005185213-c3db4df58582
	golang.org/x/net v0.60.0
	golang.org/x/sys v0.48.0
)

require (
	github.com/kr/fs v0.1.0 // indirect
	github.com/mdlayher/socket v0.6.0 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/sync v0.20.0 // indirect
)

replace fev/proto => ../proto
