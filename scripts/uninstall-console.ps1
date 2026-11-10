<#
.SYNOPSIS
    Removes the "TheLabConsole" logon task, the "Scout console" Start menu
    shortcut and any old daily "TheLabScout" task, and stops the console. Leaves
    the install folder, and the state and digests inside it, untouched.
#>
[CmdletBinding()]
param(
    [string] $InstallRoot = (Join-Path $env:LOCALAPPDATA 'thelab-scout'),
    [string] $TaskName    = 'TheLabConsole'
)

$ErrorActionPreference = 'Stop'

$lock = Join-Path $InstallRoot 'run.lock'
if ((Test-Path -LiteralPath $lock) -and ((Get-Date) - (Get-Item -LiteralPath $lock).LastWriteTime).TotalHours -lt 2) {
    throw "A run is in progress ($lock exists). Press Stop in the scout console first, then run this again."
}

foreach ($name in @($TaskName, 'TheLabScout')) {
    if (Get-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue) {
        Stop-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $name -Confirm:$false
        Write-Host "Removed scheduled task '$name'." -ForegroundColor Green
    }
}

$exePath = Join-Path $InstallRoot 'scout.exe'
Get-Process -Name scout -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -eq $exePath } | ForEach-Object { Stop-Process -Id $_.Id -Force; Write-Host 'Stopped the console.' -ForegroundColor Green }

$lnk = Join-Path ([Environment]::GetFolderPath('Programs')) 'Scout console.lnk'
if (Test-Path -LiteralPath $lnk) {
    Remove-Item -LiteralPath $lnk
    Write-Host "Removed the 'Scout console' Start menu shortcut." -ForegroundColor Green
}
