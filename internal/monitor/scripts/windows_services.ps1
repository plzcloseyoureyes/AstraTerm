# AstraTerm remote monitor — Windows service list (MON-3).
$ProgressPreference='SilentlyContinue';$ErrorActionPreference='SilentlyContinue'
ConvertTo-Json -InputObject @(Get-CimInstance Win32_Service|%{@{n=$_.Name;d=$_.DisplayName;s=$_.State;m=$_.StartMode;i=[int]$_.ProcessId;x=$_.Description}}) -Compress
