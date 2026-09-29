# NexTerm remote monitor — one-shot Windows host probe (PowerShell).
$ProgressPreference='SilentlyContinue';$ErrorActionPreference='SilentlyContinue'
$o=Get-CimInstance Win32_OperatingSystem
$c=@(Get-CimInstance Win32_Processor)
$v=Get-CimInstance Win32_ComputerSystem
@{os='Windows';h=$env:COMPUTERNAME;cap=$o.Caption;ver=$o.Version;arch=$env:PROCESSOR_ARCHITECTURE;cpu=($c|Select-Object -First 1).Name;n=[Environment]::ProcessorCount;model=$v.Model;maker=$v.Manufacturer}|ConvertTo-Json -Compress
