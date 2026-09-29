# AstraTerm remote monitor — macOS / BSD listening sockets (MON-3): sockstat (FreeBSD) or lsof for process names,
# netstat -an for the complete socket list.
export LC_ALL=C
if [ "$(uname -s)" != Darwin ] && command -v sockstat >/dev/null 2>&1; then
	echo @@SOCKSTAT
	sockstat -46l 2>/dev/null
elif command -v lsof >/dev/null 2>&1; then
	echo @@LSOFTCP
	lsof -nP -iTCP -sTCP:LISTEN -FpcLPn 2>/dev/null
	echo @@LSOFUDP
	lsof -nP -iUDP -FpcLPn 2>/dev/null
fi
echo @@NETSTAT
netstat -an 2>/dev/null | awk '/LISTEN/ || /^udp/'
echo @@END
