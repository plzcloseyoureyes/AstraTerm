# AstraTerm remote monitor — macOS / BSD process list (MON-3).
export LC_ALL=C
echo "@@SELF $$"
echo @@PS
ps -axww -o pid=,ppid=,%cpu=,%mem=,rss=,vsz=,state=,nice=,etime=,time=,user=,command= 2>/dev/null
echo @@END
