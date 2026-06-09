//go:build !(darwin && arm64)

// On every platform but darwin/arm64 the helper is not a real binary: Linux runs
// the holder in-process by re-execing the CLI itself (no separate signed VM host
// is needed), and other platforms are unsupported. This stub exists only so
// `go build ./...` resolves the package; running it errors clearly.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "fleetbox-helper is built only for darwin/arm64; this platform runs the holder in-process")
	os.Exit(1)
}
