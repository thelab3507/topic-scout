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

.PARAMETER ExportDir
    Optional. A folder OUTSIDE any git repository that receives a copy of every
    finished digest (latest.html plus one dated file per run). It is saved in
    settings.json in the install root, which is how the scheduled task (started
    with no arguments) and `scout serve` find it. Omit to leave the current
    setting alone.
#>
[CmdletBinding()]
param(
    [string] $InstallRoot = (Join-Path $env:LOCALAPPDATA 'thelab-scout'),
    [string] $TaskName    = 'TheLabScout',
    [string] $At          = '09:00',
    [string] $ExportDir   = ''
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
Write-Host "  Console:    $(Join-Path $InstallRoot 'scout.exe') serve --open   (local web page: run, next run, reports)"
if ($ExportDir) { Write-Host "  Exports:    $ExportDir" }
Write-Host ''
Write-Host 'Try it now without waiting:' -ForegroundColor Cyan
Write-Host "  Start-ScheduledTask -TaskName $TaskName"
