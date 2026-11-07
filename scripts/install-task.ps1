<#
.SYNOPSIS
    Builds scout and registers the daily "TheLabScout" scheduled task.

.DESCRIPTION
    Installs into %LOCALAPPDATA%\thelab-scout\, which is also the scout's data
    directory (digest.html, scout.log and state live next to scout.exe).

    The task runs as the current user, only while logged on, with no console
    window (wscript + run-hidden.vbs, the same pattern as the inference server).
    No elevation is needed: the server accepts loopback requests without auth.

    "Start when available" is on, so a run missed while the PC was off happens
    the next time it is on. Idempotent: re-running replaces the task and the exe.

    The one optional credential, THELAB_GITHUB_TOKEN, is read from the USER
    environment at run time.

.PARAMETER At
    Daily start time, default 09:00.
#>
[CmdletBinding()]
param(
    [string] $InstallRoot = (Join-Path $env:LOCALAPPDATA 'thelab-scout'),
    [string] $TaskName    = 'TheLabScout',
    [string] $At          = '09:00'
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'go is not on PATH; install Go or build scout.exe yourself and copy it to the install root.'
}

New-Item -ItemType Directory -Force -Path $InstallRoot | Out-Null

Push-Location $repo
try { go build -o (Join-Path $InstallRoot 'scout.exe') ./cmd/scout } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }

Copy-Item (Join-Path $repo 'sources.json')            (Join-Path $InstallRoot 'sources.json') -Force
Copy-Item (Join-Path $PSScriptRoot 'run-hidden.vbs')  (Join-Path $InstallRoot 'run-hidden.vbs') -Force

$vbs      = Join-Path $InstallRoot 'run-hidden.vbs'
$action   = New-ScheduledTaskAction -Execute 'wscript.exe' -Argument "`"$vbs`"" -WorkingDirectory $InstallRoot
$trigger  = New-ScheduledTaskTrigger -Daily -At $At
$settings = New-ScheduledTaskSettingsSet `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -ExecutionTimeLimit (New-TimeSpan -Hours 1) `
    -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $TaskName -Force -Action $action -Trigger $trigger -Settings $settings `
    -Description 'The Lab topic scout: fetch, score on the local inference server, write digest.html' | Out-Null

Write-Host "Registered '$TaskName' (daily at $At, run only when logged on)." -ForegroundColor Green
Write-Host "  Installed:  $InstallRoot"
Write-Host "  Digest:     $(Join-Path $InstallRoot 'digest.html')   (or run: scout.exe open)"
Write-Host "  Log:        $(Join-Path $InstallRoot 'scout.log')"
Write-Host ''
Write-Host 'Try it now without waiting:' -ForegroundColor Cyan
Write-Host "  Start-ScheduledTask -TaskName $TaskName"
