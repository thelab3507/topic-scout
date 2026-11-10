<#
.SYNOPSIS
    Opens the scout's web console in your browser, starting it first if needed.

.DESCRIPTION
    Works from any folder. The console normally runs all the time (the install
    script starts it at logon), so this usually just opens the page. If the
    console is not running it starts it: through the "TheLabConsole" task when
    that exists, otherwise "scout.exe serve" in a minimized window (close that
    window to stop the console). It never starts any scouting; runs begin only
    from the "Run scout" button in the page.

    The install script copies this file into the install root and adds a Start
    menu shortcut called "Scout console" that runs it.

.PARAMETER InstallRoot
    Where scout.exe, sources.json and the data live. Default: %LOCALAPPDATA%\thelab-scout.

.PARAMETER Port
    Port on 127.0.0.1. Default 8787.

.PARAMETER TaskName
    The logon task that keeps the console up. Default TheLabConsole.

.PARAMETER NoBrowser
    Start (or find) the console but do not open the browser.

.PARAMETER ShowErrors
    Report a problem in a message box. The Start menu shortcut uses this because
    its window is hidden.
#>
[CmdletBinding()]
param(
    [string] $InstallRoot = (Join-Path $env:LOCALAPPDATA 'thelab-scout'),
    [int]    $Port        = 8787,
    [string] $TaskName    = 'TheLabConsole',
    [switch] $NoBrowser,
    [switch] $ShowErrors
)

$ErrorActionPreference = 'Stop'
$exe = Join-Path $InstallRoot 'scout.exe'
$cfg = Join-Path $InstallRoot 'sources.json'
$url = "http://127.0.0.1:$Port/"

# 'scout' when the console answers, 'other' when something else holds the port, 'none' when the port is free.
function Get-PortState {
    try {
        $s = Invoke-RestMethod -Uri ($url + 'api/status') -TimeoutSec 2
        if ($null -ne $s.PSObject.Properties['has_digest']) { return 'scout' }
        return 'other'
    } catch {
        $c = New-Object System.Net.Sockets.TcpClient
        try { $c.Connect('127.0.0.1', $Port); return 'other' } catch { return 'none' } finally { $c.Close() }
    }
}

try {
    switch (Get-PortState) {
        'scout' { }
        'other' { throw "Something other than the scout console is using port $Port. Pass -Port with a free port." }
        'none' {
            if (-not (Test-Path -LiteralPath $exe)) { throw "scout.exe was not found in $InstallRoot. Run scripts\install-console.ps1 first." }
            if (-not (Test-Path -LiteralPath $cfg)) { throw "sources.json was not found in $InstallRoot. Run scripts\install-console.ps1 first." }
            # The logon task serves the default port; any other port is started directly.
            $viaTask = ($Port -eq 8787) -and (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue)
            if ($viaTask) { Start-ScheduledTask -TaskName $TaskName }
            $argList = @('serve', '--addr', "127.0.0.1:$Port", '--data-dir', "`"$InstallRoot`"", '--config', "`"$cfg`"")
            $p = $null
            if (-not $viaTask) { $p = Start-Process -FilePath $exe -ArgumentList $argList -WorkingDirectory $InstallRoot -WindowStyle Minimized -PassThru }
            $deadline = (Get-Date).AddSeconds(20)
            while ((Get-PortState) -ne 'scout') {
                if ($p -and $p.HasExited) { throw "scout serve stopped right away (exit code $($p.ExitCode)). Run '$exe serve' in a terminal to see why." }
                if ((Get-Date) -gt $deadline) { throw 'The console did not start within 20 seconds.' }
                Start-Sleep -Milliseconds 300
            }
        }
    }
    if (-not $NoBrowser) { Start-Process $url }
    Write-Host "Scout console: $url"
} catch {
    if ($ShowErrors) {
        Add-Type -AssemblyName System.Windows.Forms
        [void][System.Windows.Forms.MessageBox]::Show($_.Exception.Message, 'Scout console')
    }
    Write-Error $_
    exit 1
}
