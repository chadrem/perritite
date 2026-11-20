@echo off
rem Perritite launcher for Windows without Git Bash (cmd.exe, PowerShell).
rem launcher\perritite-fetch.ps1 installs a verified bin\perritite.exe; after
rem that, Windows finds perritite.exe ahead of this file, so only this first
rem call has its arguments parsed by cmd.exe.
setlocal
rem pwsh 7's module path breaks Windows PowerShell's own modules; let it rebuild its default.
set "PSModulePath="
rem The full path, so a powershell.exe or .bat in the current directory is never run.
%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0..\launcher\perritite-fetch.ps1"
if errorlevel 1 exit /b 1
"%~dp0perritite.exe" %*
exit /b %ERRORLEVEL%
