# Installs bin\perritite.exe for perritite.cmd: downloads this plugin
# version's Windows binary from the GitHub release and verifies it against
# launcher\SHA256SUMS (committed in the plugin) before putting it in place.
# Once bin\perritite.exe exists, Windows finds it ahead of perritite.cmd
# (PATHEXT ranks .EXE before .CMD), so later calls run the binary directly
# and their arguments never go through cmd.exe parsing.
#
# .NET is used instead of cmdlets for downloading and hashing: Windows
# PowerShell started from pwsh 7 inherits a module path that breaks
# autoloading of cmdlets such as Get-FileHash.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
function Fail($msg) { [Console]::Error.WriteLine("perritite: $msg"); exit 1 }

$repo = 'chadrem/perritite'
$root = Split-Path $PSScriptRoot -Parent
$bindir = Join-Path $root 'bin'
$exe = Join-Path $bindir 'perritite.exe'
try { $version = ([IO.File]::ReadAllText((Join-Path $root '.claude-plugin\plugin.json')) | ConvertFrom-Json).version } catch { Fail 'cannot read .claude-plugin\plugin.json' }
if (-not $version) { Fail 'no version in .claude-plugin\plugin.json' }

# A 32-bit cmd.exe starts 32-bit PowerShell, which reports x86 here; the real
# CPU is then in PROCESSOR_ARCHITEW6432.
$cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($cpu) {
  'AMD64' { $arch = 'amd64' }
  'ARM64' { $arch = 'arm64' }
  default { Fail "unsupported CPU $cpu; install with: go install github.com/$repo@latest" }
}
$asset = "perritite-windows-$arch.exe"

$want = $null
foreach ($line in [IO.File]::ReadAllLines((Join-Path $root 'launcher\SHA256SUMS'))) {
  $parts = $line.Trim() -split '\s+'
  if ($parts.Count -eq 2 -and ($parts[1] -eq $asset -or $parts[1] -eq "*$asset")) { $want = $parts[0].ToLowerInvariant(); break }
}
if (-not $want) { Fail "no checksum for $asset in launcher\SHA256SUMS" }

function Sha256($path) {
  $sha = [Security.Cryptography.SHA256]::Create()
  $stream = [IO.File]::OpenRead($path)
  try { return -join ($sha.ComputeHash($stream) | ForEach-Object { $_.ToString('x2') }) } finally { $stream.Dispose() }
}

if ([IO.File]::Exists($exe) -and (Sha256 $exe) -eq $want) { exit 0 }

$url = "https://github.com/$repo/releases/download/v$version"
if ($env:PERRITITE_RELEASE_URL) {
  # A test hook, honoured only for a local server given exactly as
  # http://127.0.0.1:<port> or http://localhost:<port>. The committed
  # checksum is still enforced.
  if ($env:PERRITITE_RELEASE_URL -cnotmatch '^http://(127\.0\.0\.1|localhost):[0-9]{1,5}\z') {
    Fail 'PERRITITE_RELEASE_URL must be exactly http://127.0.0.1:<port> or http://localhost:<port>'
  }
  $url = "$($env:PERRITITE_RELEASE_URL)/v$version"
}

$tmp = Join-Path $bindir (".download." + [guid]::NewGuid().ToString('N'))
try {
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
  try {
    (New-Object Net.WebClient).DownloadFile("$url/$asset", $tmp)
  } catch {
    Fail "could not download $url/${asset}: release v$version may not be published yet, or a network or proxy blocked it ($($_.Exception.Message))"
  }
  if ((Sha256 $tmp) -ne $want) { Fail "checksum mismatch for $asset; refusing to run it" }
  # Move, not Copy: a rename is atomic, so nobody ever sees a half-written exe.
  try {
    # A concurrent first run may have installed a good one meanwhile: keep it.
    if ([IO.File]::Exists($exe) -and (Sha256 $exe) -eq $want) { exit 0 }
    if ([IO.File]::Exists($exe)) { [IO.File]::Delete($exe) } # a stale or tampered one
    [IO.File]::Move($tmp, $exe)
  } catch {
    # A concurrent first run may have installed it (or hold it open); fine if
    # what's there verifies.
    if (-not ([IO.File]::Exists($exe) -and (Sha256 $exe) -eq $want)) { Fail "could not install ${exe}: $($_.Exception.Message)" }
  }
} finally {
  if ([IO.File]::Exists($tmp)) { [IO.File]::Delete($tmp) }
}
exit 0
