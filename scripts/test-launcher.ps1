# Tests the Windows launcher (bin/perritite.cmd + perritite-fetch.ps1) from
# PowerShell and cmd.exe against a local fake release. Windows CI only.
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
Set-Location $root
$version = (Get-Content -Raw .claude-plugin/plugin.json | ConvertFrom-Json).version
$work = Join-Path ([IO.Path]::GetTempPath()) ("perritite-launcher-" + [guid]::NewGuid().ToString('N'))
$release = Join-Path $work "release\v$version"
New-Item -ItemType Directory -Force -Path $release | Out-Null
function Fail($msg) { Write-Error "FAIL: $msg"; exit 1 }

bash scripts/build-release.sh $version ($release -replace '\\', '/')
if ($LASTEXITCODE -ne 0) { Fail 'build failed' }

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0); $listener.Start()
$port = $listener.LocalEndpoint.Port; $listener.Stop()
$server = Start-Process python -ArgumentList '-m', 'http.server', $port, '--bind', '127.0.0.1', '--directory', (Join-Path $work 'release') -PassThru -WindowStyle Hidden
try {
  for ($i = 0; $i -lt 50; $i++) { try { Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$port/" | Out-Null; break } catch { Start-Sleep -Milliseconds 200 } }
  $env:PERRITITE_RELEASE_URL = "http://127.0.0.1:$port"
  $env:LOCALAPPDATA = Join-Path $work 'appdata'
  $env:PATH = (Join-Path $root 'bin') + ';' + $env:PATH

  $out = (& perritite version) | Out-String
  if ($out.Trim() -ne "perritite $version") { Fail "PowerShell run printed '$out'" }
  Write-Output 'ok: PowerShell resolves the launcher, which downloads, verifies and runs the binary'

  $out = (cmd /c 'perritite post-flight-check "a b" 2>&1') | Out-String
  if ($out -notmatch 'unknown command "post-flight-check"') { Fail "cmd.exe did not pass arguments through intact: '$out'" }
  $out = (cmd /c 'perritite version') | Out-String
  if ($out.Trim() -ne "perritite $version") { Fail "cmd.exe run printed '$out'" }
  Write-Output 'ok: cmd.exe resolves the launcher and passes arguments through'

  (Get-Content (Join-Path $release 'SHA256SUMS')) -replace '^[0-9a-f]{8}', '00000000' | Set-Content (Join-Path $release 'SHA256SUMS')
  $env:LOCALAPPDATA = Join-Path $work 'appdata2'
  $out = (cmd /c 'perritite version 2>&1') | Out-String
  if ($LASTEXITCODE -eq 0 -or $out -notmatch 'checksum mismatch') { Fail "ran a binary whose checksum did not match: '$out'" }
  Write-Output 'ok: refuses a binary whose checksum does not match'
} finally {
  Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}

# The last native command above fails on purpose (the tamper check); the
# runner would report its exit code as this script's.
exit 0
