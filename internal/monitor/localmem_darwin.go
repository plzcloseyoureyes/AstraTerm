package monitor

import (
	"context"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// localMemory refines the memory figures of a macOS AstraTerm host: gopsutil counts inactive pages (which include idle
// app memory) as available, so its "used" disagrees with Activity Monitor and with what the remote macOS sampler
// reports for the same Mac. vm_stat gives the page counts Activity Monitor uses (see darwinMemory).
func localMemory(ctx context.Context, total int64) (MemStats, bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return MemStats{}, false
	}
	page, _ := unix.SysctlUint32("hw.pagesize")
	return darwinMemory(total, int64(page), splitLines(out))
}
