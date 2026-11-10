' Runs "scout.exe serve" (the web console) with no console window and waits for
' it, so the logon task stays "Running" for as long as the console is up. The
' first argument is the port (default 8787). This starts no scouting: runs begin
' only when you press "Run scout" in the page.
Set shell = CreateObject("WScript.Shell")
dir = CreateObject("Scripting.FileSystemObject").GetParentFolderName(WScript.ScriptFullName)
shell.CurrentDirectory = dir
port = "8787"
If WScript.Arguments.Count > 0 Then
    If IsNumeric(WScript.Arguments(0)) Then port = WScript.Arguments(0)
End If
WScript.Quit shell.Run("""" & dir & "\scout.exe"" serve --addr 127.0.0.1:" & port, 0, True)
