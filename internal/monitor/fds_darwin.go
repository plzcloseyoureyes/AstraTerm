package monitor

import "golang.org/x/sys/unix"

// localFDs reads the system-wide open file count of the AstraTerm host (kern.num_files / kern.maxfiles).
func localFDs() *FDStats {
	n, err := unix.SysctlUint32("kern.num_files")
	if err != nil {
		return nil
	}
	m, _ := unix.SysctlUint32("kern.maxfiles")
	return &FDStats{Used: int64(n), Max: int64(m)}
}
