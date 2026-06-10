//go:build fleetbox_fake

package orchestrator

// preflight is a no-op under the fleetbox_fake build tag: the fake backend boots
// no VM and touches no host network state, so it must not require /dev/kvm or
// CAP_NET_ADMIN — that lets the cross-process coordination tests run on a stock CI
// runner (ADR-0018/0020). It replaces the Linux preflight (preflight_linux.go
// carries !fleetbox_fake) when the fake tag is set.
func preflight() error {
	return nil
}
