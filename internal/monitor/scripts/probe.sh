# AstraTerm remote monitor — one-shot host probe (runs as `/bin/sh -s`): kernel, OS release, CPU model and the helper
# tools the monitor can use. Selects the per-OS sampler.
export LC_ALL=C
nx_os=$(uname -s 2>/dev/null)
echo "@@OS $nx_os"
echo "@@HOSTNAME $(uname -n 2>/dev/null)"
echo "@@KERNEL $(uname -r 2>/dev/null)"
echo "@@ARCH $(uname -m 2>/dev/null)"
case "$nx_os" in
Linux)
	echo @@RELEASE
	cat /etc/os-release 2>/dev/null || cat /usr/lib/os-release 2>/dev/null
	echo @@CPUINFO
	awk -F': *' '/^(model name|Model|Hardware|cpu model|CPU implementer|CPU part|vendor_id|machine)[ \t]*:/ { print $1 ": " $2 }' /proc/cpuinfo 2>/dev/null | sort -u
	echo @@DTMODEL
	tr -d '\000' < /sys/firmware/devicetree/base/model 2>/dev/null
	echo
	echo @@NCPU
	grep -c '^processor' /proc/cpuinfo 2>/dev/null
	echo @@VIRT
	systemd-detect-virt 2>/dev/null
	;;
Darwin)
	echo @@RELEASE
	sw_vers 2>/dev/null
	echo @@CPUMODEL
	sysctl -n machdep.cpu.brand_string 2>/dev/null || sysctl -n hw.model 2>/dev/null
	echo @@NCPU
	sysctl -n hw.ncpu 2>/dev/null
	;;
*)
	echo @@RELEASE
	freebsd-version -u 2>/dev/null
	echo @@CPUMODEL
	sysctl -n hw.model 2>/dev/null
	echo @@NCPU
	sysctl -n hw.ncpu 2>/dev/null
	;;
esac
echo "@@TOOLS sudo=$(command -v sudo 2>/dev/null) systemctl=$(command -v systemctl 2>/dev/null) journalctl=$(command -v journalctl 2>/dev/null) ss=$(command -v ss 2>/dev/null) lsof=$(command -v lsof 2>/dev/null) sockstat=$(command -v sockstat 2>/dev/null)"
echo @@END
