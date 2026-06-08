//go:build darwin && !go1.18
// +build darwin,!go1.18

package objc

import "runtime"

func SetFinalizer(obj interface{}, finalizer interface{}) {
	runtime.SetFinalizer(obj, finalizer)
}
