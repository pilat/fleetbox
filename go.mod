module github.com/pilat/fleetbox

go 1.24.0

require (
	github.com/Code-Hex/vz/v3 v3.7.1
	github.com/lima-vm/go-qcow2reader v0.7.1
	github.com/pilat/cloudiso v0.1.0
	github.com/pilat/go-ext4fs v1.0.0
	golang.org/x/crypto v0.46.0
)

require (
	github.com/Code-Hex/go-infinity-channel v1.0.0 // indirect
	golang.org/x/mod v0.22.0 // indirect
	golang.org/x/sys v0.39.0 // indirect
)

// Temporary bridge: vmnet SharedMode networking (VZVmnetNetworkDeviceAttachment)
// is not yet released upstream. We vendor norio-nomura's Code-Hex/vz PR #205
// branch (commit e27a5fb55e5936b69a62590f4ce326d8772641a9) under third_party/vz.
// Exit criterion (see docs/adr/0008): when PR #205 or its successor merges and is
// released, delete third_party/vz, drop this replace, and bump to the release.
replace github.com/Code-Hex/vz/v3 => ./third_party/vz
