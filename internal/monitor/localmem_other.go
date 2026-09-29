//go:build !darwin

package monitor

import "context"

// localMemory: gopsutil's figures are used as they are (Linux: MemAvailable; Windows: GlobalMemoryStatusEx).
func localMemory(context.Context, int64) (MemStats, bool) { return MemStats{}, false }
