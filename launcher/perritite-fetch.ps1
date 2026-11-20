# Finds or downloads the perritite.exe for this plugin's version, verifies it
# against the release's SHA256SUMS, and prints its path (used by perritite.cmd).
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
function Fail($msg) { [Console]::Error.WriteLine("perritite: $msg"); exit 1 }

$repo = 'chadrem/perritite'
$manifest = Join-Path (Split-Path $PSScriptRoot -Parent) '.claude-plugin\plugin.json'
try { $version = (Get-Content -Raw $manifest | ConvertFrom-Json).version } catch { Fail "cannot read $manifest" }
if (-not $version) { Fail "no version in $manifest" }

switch ($env:PROCESSOR_ARCHITECTURE) {
  'AMD64' { $arch = 'amd64' }
  'ARM64' { $arch = 'arm64' }
  default { Fail "unsupported CPU $($env:PROCESSOR_ARCHITECTURE); install with: go install github.com/$repo@latest" }
}
$asset = "perritite-windows-$arch.exe"
$dir = Join-Path $env:LOCALAPPDATA "perritite\bin\$version"
$bin = Join-Path $dir $asset

if (-not (Test-Path $bin)) {
  $base = "https://github.com/$repo/releases/download"
  if ($env:PERRITITE_RELEASE_URL) {
    # A test hook, honoured only for a local server: a downloaded binary is
    # executed, so it must never come from anywhere but the release.
    if ($env:PERRITITE_RELEASE_URL -notmatch '^http://(127\.0\.0\.1|localhost):\d+') {
      Fail 'PERRITITE_RELEASE_URL must be http://127.0.0.1:<port> or http://localhost:<port>'
    }
    $base = $env:PERRITITE_RELEASE_URL
  }
  $url = "$base/v$version"
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $tmp = Join-Path $dir (".download." + [guid]::NewGuid().ToString('N'))
  try {
    Invoke-WebRequest -UseBasicParsing -Uri "$url/$asset" -OutFile $tmp
    $sums = (Invoke-WebRequest -UseBasicParsing -Uri "$url/SHA256SUMS").Content
    if ($sums -is [byte[]]) { $sums = [Text.Encoding]::UTF8.GetString($sums) }
    $want = $null
    foreach ($line in ($sums -split "`n")) {
      $parts = $line.Trim() -split '\s+'
      if ($parts.Count -eq 2 -and ($parts[1] -eq $asset -or $parts[1] -eq "*$asset")) { $want = $parts[0] }
    }
    if (-not $want) { Fail "no checksum for $asset in SHA256SUMS" }
    $got = (Get-FileHash -Algorithm SHA256 $tmp).Hash
    if ($got -ne $want.ToUpper()) { Fail "checksum mismatch for $asset; refusing to run it" }
    Move-Item -Force $tmp $bin
  } catch {
    Fail "could not download $url/${asset}: $($_.Exception.Message)"
  } finally {
    if (Test-Path $tmp) { Remove-Item -Force $tmp }
  }
}
Write-Output $bin
