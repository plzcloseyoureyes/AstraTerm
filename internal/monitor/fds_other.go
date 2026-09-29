//go:build !linux && !darwin

package monitor

// localFDs is not available on this platform.
func localFDs() *FDStats { return nil }
