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
    # .NET rather than cmdlets for download and hashing: Windows PowerShell
    # started from pwsh 7 inherits a module path that breaks autoloading of
    # cmdlets such as Get-FileHash.
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $client = New-Object Net.WebClient
    try {
      $client.DownloadFile("$url/$asset", $tmp)
      $sums = $client.DownloadString("$url/SHA256SUMS")
    } catch {
      Fail "could not download from ${url}: $($_.Exception.Message)"
    }
    $want = $null
    foreach ($line in ($sums -split "`n")) {
      $parts = $line.Trim() -split '\s+'
      if ($parts.Count -eq 2 -and ($parts[1] -eq $asset -or $parts[1] -eq "*$asset")) { $want = $parts[0] }
    }
    if (-not $want) { Fail "no checksum for $asset in SHA256SUMS" }
    $sha = [Security.Cryptography.SHA256]::Create()
    $stream = [IO.File]::OpenRead($tmp)
    try { $got = -join ($sha.ComputeHash($stream) | ForEach-Object { $_.ToString('x2') }) } finally { $stream.Dispose() }
    if ($got -ne $want.ToLowerInvariant()) { Fail "checksum mismatch for $asset; refusing to run it" }
    Move-Item -Force $tmp $bin
  } finally {
    if (Test-Path $tmp) { Remove-Item -Force $tmp }
  }
}
Write-Output $bin
