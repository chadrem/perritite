@echo off
rem Perritite launcher for Windows without Git Bash (cmd.exe, PowerShell).
rem launcher\perritite-fetch.ps1 finds or downloads the verified binary and prints its
rem path; arguments go straight to the binary, never through PowerShell.
setlocal
rem pwsh 7's module path breaks Windows PowerShell's own modules; let it rebuild its default.
set "PSModulePath="
set "PERRITITE_EXE="
for /f "usebackq delims=" %%B in (`powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0..\launcher\perritite-fetch.ps1"`) do set "PERRITITE_EXE=%%B"
if not defined PERRITITE_EXE exit /b 1
"%PERRITITE_EXE%" %*
exit /b %ERRORLEVEL%
