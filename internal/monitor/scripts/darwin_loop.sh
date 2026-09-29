# Termstead remote monitor — macOS sampler (MON-2). Same framing as the Linux sampler: one sample between @@S and @@E
# every __INTERVAL__ s, __COUNT__ samples (0 = forever). CPU usage comes from a 1 s iostat window, memory from vm_stat,
# network counters from netstat -ibn, disks from df -kP -l (local volumes only).
export LC_ALL=C
nx_n=0
while [ __COUNT__ -eq 0 ] || [ "$nx_n" -lt __COUNT__ ]; do
	echo @@S
	echo @@SYSCTL
	sysctl hw.ncpu hw.memsize hw.pagesize vm.loadavg kern.boottime vm.swapusage kern.hostname kern.num_files kern.maxfiles 2>/dev/null
	echo "@@NOW $(date +%s)"
	echo @@IOSTAT
	iostat -n0 -c2 -w1 2>/dev/null | tail -1
	echo @@VM
	vm_stat 2>/dev/null
	echo @@NET
	netstat -ibn 2>/dev/null
	echo @@MOUNT
	mount 2>/dev/null
	echo @@DF
	df -kP -l 2>/dev/null
	echo @@USERS
	who 2>/dev/null | wc -l
	echo @@PROCS
	ps -A -o pid= 2>/dev/null | wc -l
	echo @@E
	nx_n=$((nx_n + 1))
	if [ __COUNT__ -ne 0 ] && [ "$nx_n" -ge __COUNT__ ]; then break; fi
	if [ "$nx_n" -gt 1 ]; then sleep __SLEEP__; fi
done </dev/null
