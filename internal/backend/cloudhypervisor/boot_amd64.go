//go:build linux && amd64

package cloudhypervisor

// bootCmdline is the kernel command line for a direct boot of fleetbox's catalog
// cloud image on x86_64. console=ttyS0 targets the 16550 UART cloud-hypervisor
// exposes on x86_64 (so the serial log fills); root is the first virtio-blk
// partition — the disk is the first --disk value (vda) and every catalog image puts
// root on p1. The seed and fixtures are later --disk values (vdb+), mounted by
// LABEL, so they do not affect root=. Deriving root= from an arbitrary image is
// deferred (ADR-0029).
const bootCmdline = "console=ttyS0 root=/dev/vda1 rw"
