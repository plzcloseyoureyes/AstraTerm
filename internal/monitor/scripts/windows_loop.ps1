# AstraTerm remote monitor — Windows (OpenSSH + PowerShell) sampler (MON-2). One compact JSON object per line every
# __INTERVAL__ s, __COUNT__ samples (0 = forever). Raw perf counters: the Go side computes CPU/network deltas.
$ProgressPreference='SilentlyContinue';$ErrorActionPreference='SilentlyContinue'
$n=0
while(__COUNT__ -eq 0 -or $n -lt __COUNT__){
$o=Get-CimInstance Win32_OperatingSystem
$c=@(Get-CimInstance Win32_PerfRawData_PerfOS_Processor|%{@{n=$_.Name;i=[uint64]$_.PercentProcessorTime;u=[uint64]$_.PercentUserTime;k=[uint64]$_.PercentPrivilegedTime;ts=[uint64]$_.Timestamp_Sys100NS}})
$e=@(Get-CimInstance Win32_PerfRawData_Tcpip_NetworkInterface|%{@{n=$_.Name;r=[uint64]$_.BytesReceivedPersec;t=[uint64]$_.BytesSentPersec}})
$d=@(Get-CimInstance Win32_LogicalDisk -Filter 'DriveType=3'|%{@{m=$_.DeviceID;f=$_.FileSystem;s=[uint64]$_.Size;v=[uint64]$_.FreeSpace}})
$p=@(Get-CimInstance Win32_PageFileUsage)
$q=Get-CimInstance Win32_PerfRawData_PerfOS_System
$u=0;try{$u=@(quser 2>$null|Select-Object -Skip 1).Count}catch{}
@{h=$env:COMPUTERNAME;up=[int64]((Get-Date)-$o.LastBootUpTime).TotalSeconds;mt=[uint64]$o.TotalVisibleMemorySize*1024;mf=[uint64]$o.FreePhysicalMemory*1024;pt=[uint64](($p|Measure-Object AllocatedBaseSize -Sum).Sum)*1048576;pu=[uint64](($p|Measure-Object CurrentUsage -Sum).Sum)*1048576;np=[int]$o.NumberOfProcesses;th=[int]$q.Threads;ql=[int]$q.ProcessorQueueLength;cpu=$c;net=$e;dk=$d;u=$u}|ConvertTo-Json -Compress -Depth 4
$n++
if(__COUNT__ -ne 0 -and $n -ge __COUNT__){break}
if($n -eq 1){Start-Sleep -Seconds 1}else{Start-Sleep -Seconds __INTERVAL__}
}
