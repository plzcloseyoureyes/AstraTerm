# NexTerm remote monitor — FreeBSD / OpenBSD / NetBSD / DragonFly sampler (MON-2). CPU from kern.cp_time(s) tick
# counters, memory from vm.stats (FreeBSD) or vmstat -s, swap from swapinfo / swapctl, network from netstat -ibn.
export LC_ALL=C
nx_n=0
while [ __COUNT__ -eq 0 ] || [ "$nx_n" -lt __COUNT__ ]; do
	echo @@S
	echo @@SYSCTL
	for nx_k in kern.cp_time kern.cp_times hw.ncpu hw.physmem hw.pagesize vm.loadavg kern.boottime kern.hostname \
		vm.stats.vm.v_page_count vm.stats.vm.v_free_count vm.stats.vm.v_inactive_count vm.stats.vm.v_cache_count \
		vm.stats.vm.v_laundry_count vm.stats.vm.v_active_count vm.stats.vm.v_wire_count kern.openfiles kern.maxfiles \
		kern.nfiles; do
		sysctl "$nx_k" 2>/dev/null
	done
	echo "@@NOW $(date +%s)"
	echo @@VMSTAT
	vmstat -s 2>/dev/null
	echo @@SWAP
	swapinfo -k 2>/dev/null || swapctl -lk 2>/dev/null
	echo @@NET
	netstat -ibn 2>/dev/null
	echo @@MOUNT
	mount 2>/dev/null
	echo @@DF
	df -kP -l 2>/dev/null || df -kP 2>/dev/null
	echo @@USERS
	who 2>/dev/null | wc -l
	echo @@PROCS
	ps -ax -o pid= 2>/dev/null | wc -l
	echo @@E
	nx_n=$((nx_n + 1))
	if [ __COUNT__ -ne 0 ] && [ "$nx_n" -ge __COUNT__ ]; then break; fi
	if [ "$nx_n" -eq 1 ]; then sleep 1; else sleep __INTERVAL__; fi
done </dev/null
