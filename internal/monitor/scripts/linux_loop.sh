# Termstead remote monitor — Linux sampler (RESEARCH §3.21). Runs as `/bin/sh -s` on one non-PTY exec channel and prints
# one sample every __INTERVAL__ s between @@S and @@E markers; __COUNT__ samples (0 = until the channel closes).
# Only /proc reads and a few tiny commands per sample (BusyBox compatible). Network / pseudo filesystems are never
# passed to df, so a hung NFS server cannot stall the loop; neither are the mounts of container runtimes (a Docker or
# Kubernetes host has one per container) and system runtime trees (removable media under /run/media stay).
export LC_ALL=C
IFS='
'
nx_mounts() {
	awk '$3 ~ /^(proc|sysfs|devtmpfs|devpts|tmpfs|cgroup|cgroup2|pstore|bpf|tracefs|debugfs|securityfs|configfs|fusectl|mqueue|hugetlbfs|autofs|binfmt_misc|rpc_pipefs|nsfs|efivarfs|ramfs|squashfs|selinuxfs|nfsd|nfs|nfs4|cifs|smb3|smbfs|9p|virtiofs|ceph|glusterfs|afs|lustre|gpfs|iso9660|udf|fuse\..*)$/ { next }
	{ gsub(/\\040/, " ", $2) }
	$2 ~ /^\/(proc|sys|dev|snap)(\/|$)/ { next }
	$2 ~ /^\/run(\/|$)/ && $2 !~ /^\/run\/media\// { next }
	$2 ~ /^\/var\/(lib\/(docker|containers|kubelet|lxcfs)|snap)(\/|$)/ { next }
	{ print $3 "\t" $2 }' /proc/mounts 2>/dev/null
}
nx_n=0
while [ __COUNT__ -eq 0 ] || [ "$nx_n" -lt __COUNT__ ]; do
	echo @@S
	echo @@UPTIME
	cat /proc/uptime
	echo @@LOAD
	cat /proc/loadavg
	echo @@CPU
	grep '^cpu' /proc/stat
	echo @@MEM
	grep -E '^(MemTotal|MemFree|MemAvailable|Buffers|Cached|SwapTotal|SwapFree|Shmem|SReclaimable):' /proc/meminfo
	echo @@NET
	cat /proc/net/dev
	echo @@DISKIO
	awk '$3 !~ /^(loop|ram|zram|sr|fd|nbd)/' /proc/diskstats 2>/dev/null
	echo @@FD
	cat /proc/sys/fs/file-nr 2>/dev/null
	echo @@HOST
	cat /proc/sys/kernel/hostname 2>/dev/null
	echo @@VNET
	ls /sys/devices/virtual/net 2>/dev/null
	nx_m=$(nx_mounts)
	echo @@MOUNTS
	printf '%s\n' "$nx_m"
	echo @@DF
	nx_d=$(printf '%s\n' "$nx_m" | cut -f2-)
	if [ -n "$nx_d" ]; then df -kP $nx_d 2>/dev/null; else df -kP 2>/dev/null; fi
	echo @@USERS
	who 2>/dev/null | wc -l
	set -- /proc/[0-9]*
	echo "@@PROCS $#"
	echo @@E
	nx_n=$((nx_n + 1))
	if [ __COUNT__ -ne 0 ] && [ "$nx_n" -ge __COUNT__ ]; then break; fi
	if [ "$nx_n" -eq 1 ]; then sleep 1; else sleep __INTERVAL__; fi
done </dev/null
