package monitor

import (
	"os"
	"strings"
)

// localFDs reads the system-wide open file handles of the AstraTerm host (/proc/sys/fs/file-nr).
func localFDs() *FDStats {
	b, err := os.ReadFile("/proc/sys/fs/file-nr")
	if err != nil {
		return nil
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return nil
	}
	maxfd := atoi64(f[2])
	if maxfd <= 0 || maxfd >= 1<<62 {
		maxfd = 0
	}
	return &FDStats{Used: max(atoi64(f[0])-atoi64(f[1]), 0), Max: maxfd}
}
