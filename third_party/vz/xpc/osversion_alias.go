//go:build darwin

package xpc

import (
	"github.com/pilat/fleetbox/third_party/vz/internal/osversion"
)

var macOSAvailable = osversion.MacOSAvailable
