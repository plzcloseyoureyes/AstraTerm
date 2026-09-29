# Termstead remote monitor — Linux process list (MON-3). Raw /proc/<pid>/stat lines (CPU ticks, state, RSS, threads,
# start time) joined with ps for the user and full command line. Works with procps and BusyBox.
export LC_ALL=C
echo "@@SELF $$"
echo "@@HZ $(getconf CLK_TCK 2>/dev/null || echo 100)"
echo "@@PAGE $(getconf PAGESIZE 2>/dev/null || echo 4096)"
read nx_up nx_idle < /proc/uptime
echo "@@UPTIME $nx_up"
echo "@@MEMTOTAL $(awk '/^MemTotal:/ { print $2; exit }' /proc/meminfo)"
echo @@STAT
cat /proc/[0-9]*/stat 2>/dev/null
echo
echo @@PS
ps -ww -eo pid=,user:32=,args= 2>/dev/null || ps -o pid,user,args 2>/dev/null
echo @@END
