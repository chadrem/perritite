# Tests the Windows launcher (bin\perritite.cmd + launcher\perritite-fetch.ps1)
# from PowerShell and cmd.exe against a local fake release, using a throwaway
# copy of the plugin with its own launcher\SHA256SUMS. Windows CI only.
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent
$version = '0.0.1'
$work = Join-Path ([IO.Path]::GetTempPath()) ("perritite-launcher-" + [guid]::NewGuid().ToString('N'))
$plugin = Join-Path $work 'plugin'
$release = Join-Path $work "release\v$version"
function Fail($msg) { Write-Error "FAIL: $msg"; exit 1 }

New-Item -ItemType Directory -Force -Path (Join-Path $plugin '.claude-plugin'), (Join-Path $plugin 'launcher'), $release | Out-Null
Copy-Item -Recurse (Join-Path $root 'bin') (Join-Path $plugin 'bin')
Copy-Item (Join-Path $root 'launcher\perritite-fetch.ps1') (Join-Path $plugin 'launcher')
Remove-Item -Force -ErrorAction SilentlyContinue (Join-Path $plugin 'bin\perritite.exe')
Set-Content -NoNewline (Join-Path $plugin '.claude-plugin\plugin.json') "{ `"name`": `"perritite`", `"version`": `"$version`" }"

bash (Join-Path $root 'scripts/build-release.sh') $version ($release -replace '\\', '/')
if ($LASTEXITCODE -ne 0) { Fail 'build failed' }
Copy-Item (Join-Path $release 'SHA256SUMS') (Join-Path $plugin 'launcher\SHA256SUMS')

$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0); $listener.Start()
$port = $listener.LocalEndpoint.Port; $listener.Stop()
$server = Start-Process python -ArgumentList '-m', 'http.server', $port, '--bind', '127.0.0.1', '--directory', (Join-Path $work 'release') -PassThru -WindowStyle Hidden
try {
  for ($i = 0; $i -lt 50; $i++) { try { Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$port/" | Out-Null; break } catch { Start-Sleep -Milliseconds 200 } }
  $env:PERRITITE_RELEASE_URL = "http://127.0.0.1:$port"
  $env:PATH = (Join-Path $plugin 'bin') + ';' + $env:PATH

  $out = (& perritite version) | Out-String
  if ($out.Trim() -ne "perritite $version") { Fail "first PowerShell run printed '$out'" }
  if (-not (Test-Path -LiteralPath (Join-Path $plugin 'bin\perritite.exe'))) { Fail 'bin\perritite.exe was not installed' }
  Write-Output 'ok: the first run downloads, verifies and installs bin\perritite.exe'

  $resolved = (Get-Command perritite).Source
  if ($resolved -notlike '*perritite.exe') { Fail "later calls resolve to $resolved, not perritite.exe" }
  $out = (& perritite no-such-command 'a&b "c"' 2>&1) | Out-String
  if ($out -notmatch [regex]::Escape('unknown command "no-such-command"')) { Fail "PowerShell did not pass arguments through intact: '$out'" }
  $out = (cmd /c 'perritite no-such-command "a&b" 2>&1') | Out-String
  if ($out -notmatch [regex]::Escape('unknown command "no-such-command"')) { Fail "cmd.exe did not pass arguments through intact: '$out'" }
  Write-Output 'ok: later calls go straight to perritite.exe, with & and quotes intact'

  # A tampered installed exe is replaced, never run.
  Set-Content -LiteralPath (Join-Path $plugin 'bin\perritite.exe') 'garbage'
  $out = (cmd /c (Join-Path $plugin 'bin\perritite.cmd') version) | Out-String
  if ($out.Trim() -ne "perritite $version") { Fail "after tampering, printed '$out'" }
  Write-Output 'ok: a tampered perritite.exe is replaced by a verified one'

  # A release that doesn't match the committed checksum is refused.
  Get-ChildItem -LiteralPath $release -Filter 'perritite-*' | ForEach-Object { Set-Content -LiteralPath $_.FullName 'garbage' }
  Remove-Item -Force -LiteralPath (Join-Path $plugin 'bin\perritite.exe')
  $out = (cmd /c ((Join-Path $plugin 'bin\perritite.cmd') + ' version 2>&1')) | Out-String
  if ($LASTEXITCODE -eq 0 -or $out -notmatch 'checksum mismatch') { Fail "ran a binary that does not match the committed checksum: '$out'" }
  if (Test-Path -LiteralPath (Join-Path $plugin 'bin\perritite.exe')) { Fail 'left an unverified perritite.exe behind' }
  Write-Output 'ok: refuses a download that does not match the committed checksum'

  foreach ($bad in @('https://evil.example', "http://localhost:1@127.0.0.1:$port", "http://127.0.0.1:${port}@evil.example")) {
    $env:PERRITITE_RELEASE_URL = $bad
    $out = (cmd /c ((Join-Path $plugin 'bin\perritite.cmd') + ' version 2>&1')) | Out-String
    if ($LASTEXITCODE -eq 0 -or $out -notmatch 'must be exactly') { Fail "accepted release URL $bad`: '$out'" }
  }
  Write-Output 'ok: refuses non-local and userinfo-smuggled download URLs'
} finally {
  Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
  Remove-Item -Recurse -Force -LiteralPath $work -ErrorAction SilentlyContinue
}

# The last native commands above fail on purpose; the runner would report
# their exit code as this script's.
exit 0
