# AstraTerm remote monitor — Windows listening sockets (MON-3).
$ProgressPreference='SilentlyContinue';$ErrorActionPreference='SilentlyContinue'
$n=@{};Get-Process|%{$n[[int]$_.Id]=$_.ProcessName}
$r=@()
Get-NetTCPConnection -State Listen|%{$r+=@{pr='tcp';a=$_.LocalAddress;p=[int]$_.LocalPort;i=[int]$_.OwningProcess;c=$n[[int]$_.OwningProcess]}}
Get-NetUDPEndpoint|%{$r+=@{pr='udp';a=$_.LocalAddress;p=[int]$_.LocalPort;i=[int]$_.OwningProcess;c=$n[[int]$_.OwningProcess]}}
ConvertTo-Json -InputObject @($r) -Compress
