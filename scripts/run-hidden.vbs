' Runs scout.exe with no console window and hands its exit code back to
' Task Scheduler, so "Last Run Result" shows 0 / 1 / 2 (see README).
Set shell = CreateObject("WScript.Shell")
dir = CreateObject("Scripting.FileSystemObject").GetParentFolderName(WScript.ScriptFullName)
shell.CurrentDirectory = dir
WScript.Quit shell.Run("""" & dir & "\scout.exe"" run", 0, True)
