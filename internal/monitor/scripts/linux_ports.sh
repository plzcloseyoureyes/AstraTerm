# AstraTerm remote monitor — Linux listening sockets (MON-3): ss, else netstat, plus /proc/net as the fallback.
export LC_ALL=C
if command -v ss >/dev/null 2>&1; then
	echo @@SS
	ss -Hltnup 2>/dev/null || ss -ltnup 2>/dev/null
elif netstat -ltnup >/dev/null 2>&1; then
	echo @@NETSTAT
	netstat -ltnup 2>/dev/null
fi
echo @@COMM
for nx_f in /proc/[0-9]*/comm; do
	nx_p=${nx_f#/proc/}
	nx_p=${nx_p%/comm}
	read -r nx_c 2>/dev/null <"$nx_f" && echo "$nx_p $nx_c"
done 2>/dev/null
echo @@PROC
for nx_f in tcp tcp6 udp udp6; do
	echo "##F $nx_f"
	cat "/proc/net/$nx_f" 2>/dev/null
done
echo @@END
