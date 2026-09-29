# NexTerm remote monitor — Windows process list (MON-3).
$ProgressPreference='SilentlyContinue';$ErrorActionPreference='SilentlyContinue'
$m=[int64](Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory
$w=@{};try{Get-Process -IncludeUserName -ErrorAction Stop|%{$w[[int]$_.Id]=$_.UserName}}catch{}
$t=Get-Date
$l=@(Get-CimInstance Win32_Process|%{@{p=[int]$_.ProcessId;pp=[int]$_.ParentProcessId;n=$_.Name;c=$_.CommandLine;ws=[int64]$_.WorkingSetSize;vs=[int64]$_.VirtualSize;cpu=[int64]$_.KernelModeTime+[int64]$_.UserModeTime;th=[int]$_.ThreadCount;pr=[int]$_.Priority;s=$(if($_.CreationDate){[int64]($t-$_.CreationDate).TotalSeconds}else{-1});u=$w[[int]$_.ProcessId]}})
@{mem=$m;self=$PID;procs=$l}|ConvertTo-Json -Compress -Depth 3
