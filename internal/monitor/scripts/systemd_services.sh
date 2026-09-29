# NexTerm remote monitor — systemd service list (MON-3): loaded units plus unit-file enablement states.
export LC_ALL=C
echo @@UNITS
systemctl list-units --type=service --all --no-legend --no-pager --plain 2>/dev/null
echo @@FILES
systemctl list-unit-files --type=service --no-legend --no-pager 2>/dev/null
echo @@END
