<#
.SYNOPSIS
    Builds scout and keeps its web console available whenever you are logged on.

.DESCRIPTION
    The scout never runs on a schedule. It runs only when you press "Run scout"
    in the web console (and stops when you press "Stop"). This script makes the
    console itself always available:

      * installs scout.exe, sources.json and the launcher into
        %LOCALAPPDATA%\thelab-scout\ (also the scout's data directory),
      * registers the "TheLabConsole" task, which starts "scout.exe serve" hidden
        at every logon of the current user (and restarts it if it crashes),
      * adds a Start menu shortcut "Scout console" that opens the page,
      * removes the old daily "TheLabScout" task, if you have it,
      * starts the console now.

    The task runs only the web server. It starts no scouting. Idempotent:
    re-running replaces the task and the exe. Re-running refuses to start while a
    run is in progress; press Stop in the console first.

.PARAMETER ExportDir
    Optional. A folder OUTSIDE any git repository that receives a copy of every
    finished digest (latest.html plus one dated file per run). It is saved in
    settings.json in the install root. Omit to leave the current setting alone.

.PARAMETER Port
    Port for the console on 127.0.0.1. Default 8787.
#>
[CmdletBinding()]
param(
    [string] $InstallRoot = (Join-Path $env:LOCALAPPDATA 'thelab-scout'),
    [string] $TaskName    = 'TheLabConsole',
    [int]    $Port        = 8787,
    [string] $ExportDir   = ''
)

$ErrorActionPreference = 'Stop'
$repo      = Split-Path -Parent $PSScriptRoot
$oldTask   = 'TheLabScout'   # the daily task of earlier versions
$exePath   = Join-Path $InstallRoot 'scout.exe'
$url       = "http://127.0.0.1:$Port/"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'go is not on PATH; install Go or build scout.exe yourself and copy it to the install root.'
}

# Never pull the exe out from under a run that is scoring.
$lock = Join-Path $InstallRoot 'run.lock'
if ((Test-Path -LiteralPath $lock) -and ((Get-Date) - (Get-Item -LiteralPath $lock).LastWriteTime).TotalHours -lt 2) {
    throw "A run is in progress ($lock exists). Press Stop in the scout console (or wait), then run this again."
}

New-Item -ItemType Directory -Force -Path $InstallRoot | Out-Null

# Stop what is running from the install root so the exe can be replaced.
foreach ($name in @($TaskName, $oldTask)) {
    if (Get-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue) { Stop-ScheduledTask -TaskName $name -ErrorAction SilentlyContinue }
}
Get-Process -Name scout -ErrorAction SilentlyContinue |
    Where-Object { $_.Path -eq $exePath } | ForEach-Object { Stop-Process -Id $_.Id -Force; $_.WaitForExit(5000) | Out-Null }

Push-Location $repo
try { go build -o $exePath ./cmd/scout } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }

Copy-Item (Join-Path $repo 'sources.json')               (Join-Path $InstallRoot 'sources.json') -Force
Copy-Item (Join-Path $PSScriptRoot 'run-hidden.vbs')     (Join-Path $InstallRoot 'run-hidden.vbs') -Force
Copy-Item (Join-Path $PSScriptRoot 'open-console.ps1')   (Join-Path $InstallRoot 'open-console.ps1') -Force

if ($ExportDir) {
    $ExportDir = [System.IO.Path]::GetFullPath($ExportDir)
    for ($p = $ExportDir; $p; $p = Split-Path -Parent $p) {
        if (Test-Path -LiteralPath (Join-Path $p '.git')) {
            throw "$ExportDir is inside a git repository ($p). Choose a folder outside every repo."
        }
    }
    New-Item -ItemType Directory -Force -Path $ExportDir | Out-Null
    $settingsPath = Join-Path $InstallRoot 'settings.json'
    $conf = [ordered]@{}
    if (Test-Path -LiteralPath $settingsPath) {
        (Get-Content -Raw -LiteralPath $settingsPath | ConvertFrom-Json).PSObject.Properties | ForEach-Object { $conf[$_.Name] = $_.Value }
    }
    $conf['export_dir'] = $ExportDir
    # UTF-8 without a BOM (Windows PowerShell 5.1's -Encoding UTF8 adds one).
    [System.IO.File]::WriteAllText($settingsPath, ($conf | ConvertTo-Json), (New-Object System.Text.UTF8Encoding $false))
}

# The daily schedule of earlier versions is gone: the scout runs only on request.
if (Get-ScheduledTask -TaskName $oldTask -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $oldTask -Confirm:$false
    Write-Host "Removed the old daily task '$oldTask'. The scout now runs only when you start it in the console." -ForegroundColor Yellow
}

# Start menu shortcut: search "Scout console" (or pin it) to open the page from anywhere.
$launcher = Join-Path $InstallRoot 'open-console.ps1'
$lnkPath  = Join-Path ([Environment]::GetFolderPath('Programs')) 'Scout console.lnk'
$lnk = (New-Object -ComObject WScript.Shell).CreateShortcut($lnkPath)
$lnk.TargetPath       = 'powershell.exe'
$lnk.Arguments        = "-NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$launcher`" -InstallRoot `"$InstallRoot`" -Port $Port -ShowErrors"
$lnk.WorkingDirectory = $InstallRoot
$lnk.Description      = 'Open the Lab topic scout web console'
$lnk.Save()

# The console starts at logon, hidden, and is restarted if it ever crashes. It
# only serves the page; it starts no scouting.
$user     = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
$vbs      = Join-Path $InstallRoot 'run-hidden.vbs'
$action   = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument "`"$vbs`" $Port" -WorkingDirectory $InstallRoot
$trigger  = New-ScheduledTaskTrigger -AtLogOn -User $user
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -ExecutionTimeLimit ([TimeSpan]::Zero) `
    -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) `
    -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $TaskName -Force -Action $action -Trigger $trigger -Settings $settings `
    -Description 'The Lab topic scout: the web console, always available while logged on. It starts no scouting by itself.' | Out-Null
Start-ScheduledTask -TaskName $TaskName

Write-Host "Registered '$TaskName' (starts the console hidden at logon) and started it." -ForegroundColor Green
Write-Host "  Installed:  $InstallRoot"
Write-Host "  Console:    $url   (Start menu > 'Scout console')"
Write-Host "  Log:        $(Join-Path $InstallRoot 'scout.log')"
if ($ExportDir) { Write-Host "  Exports:    $ExportDir" }
Write-Host ''
Write-Host 'Start and stop the scout from the console page. Nothing runs on a schedule.' -ForegroundColor Cyan
