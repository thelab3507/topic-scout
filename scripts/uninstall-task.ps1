<#
.SYNOPSIS
    Removes the "TheLabScout" scheduled task. Leaves the install folder, and the
    state and digest inside it, untouched.
#>
[CmdletBinding()]
param([string] $TaskName = 'TheLabScout')

$ErrorActionPreference = 'Stop'
if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
    Write-Host "Removed scheduled task '$TaskName'." -ForegroundColor Green
} else {
    Write-Host "No scheduled task named '$TaskName'."
}
