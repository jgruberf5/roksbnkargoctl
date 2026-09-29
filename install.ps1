<#
.SYNOPSIS
  roksbnkargoctl installer for Windows.

.DESCRIPTION
  Downloads the release .zip for this architecture and BNK version, verifies its
  SHA256 against the release's checksums file, extracts roksbnkargoctl.exe, and
  hands off to the binary's own `roksbnkargoctl self install --force`, which
  copies it onto PATH. The temp directory holding the archive and the extracted
  binary is removed afterwards, leaving only the installed copy.

  Each binary installs one BNK release, and the archives are named for it:
    roksbnkargoctl_<version>_bnk-<BNK version>_windows_<arch>.zip

  Run it directly:
    irm https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.ps1 | iex

  Options (environment; the ROKSBNKARGOCTL_ names win over the short ones):
    $env:ROKSBNKARGOCTL_VERSION (or VERSION)          install that release, e.g. 'v0.5.0'
    $env:ROKSBNKARGOCTL_BNK_VERSION (or BNK_VERSION)  the BNK release (default 2.4.0)
    $env:ROKSBNKARGOCTL_INSTALL_DIR                   where `self install` puts it (spaces OK)
    $env:ROKSBNKARGOCTL_INSTALL_ARGS                  more `self install` arguments
    $env:GITHUB_TOKEN                                 authenticates the GitHub API call

  Everything runs inside a script block, so `irm | iex` leaves no variables or
  preference changes behind in your session.

  The checksum is mandatory: a release without a checksums file, or one that does
  not list the archive, is refused.
#>
& {
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Repo = 'jgruberf5/roksbnkargoctl'
$Bin = 'roksbnkargoctl'
$Bnk = if ($env:ROKSBNKARGOCTL_BNK_VERSION) { $env:ROKSBNKARGOCTL_BNK_VERSION } elseif ($env:BNK_VERSION) { $env:BNK_VERSION } else { '2.4.0' }
$Version = if ($env:ROKSBNKARGOCTL_VERSION) { $env:ROKSBNKARGOCTL_VERSION } elseif ($env:VERSION) { $env:VERSION } else { '' }
$InstallArgs = if ($env:ROKSBNKARGOCTL_INSTALL_ARGS) { $env:ROKSBNKARGOCTL_INSTALL_ARGS } else { '' }
$InstallDir = if ($env:ROKSBNKARGOCTL_INSTALL_DIR) { $env:ROKSBNKARGOCTL_INSTALL_DIR } else { '' }
$Api = if ($env:ROKSBNKARGOCTL_GITHUB_API) { $env:ROKSBNKARGOCTL_GITHUB_API } else { 'https://api.github.com' }

if ($Bnk -notmatch '^[0-9][0-9.]*$') { throw "${Bin}: BNK_VERSION '$Bnk' is not a version like 2.4.0" }

# ---- architecture (goreleaser naming) --------------------------------------
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
  'ARM64' { 'arm64' }
  'AMD64' { 'amd64' }
  default { throw "${Bin}: unsupported architecture '$($env:PROCESSOR_ARCHITECTURE)' (amd64 and arm64 only)" }
}

# ---- resolve the release ----------------------------------------------------
$headers = @{ 'User-Agent' = $Bin; 'Accept' = 'application/vnd.github+json' }
if ($env:GITHUB_TOKEN) { $headers['Authorization'] = "Bearer $($env:GITHUB_TOKEN)" }
if ($Version) {
  if (-not $Version.StartsWith('v')) { $Version = "v$Version" }
  # A release tag only: it goes into the API URL.
  if ($Version -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') { throw "${Bin}: version '$Version' is not a release tag like v0.5.0" }
  $url = "$Api/repos/$Repo/releases/tags/$Version"
} else {
  $url = "$Api/repos/$Repo/releases/latest"
}
$release = Invoke-RestMethod -Uri $url -Headers $headers
$tag = $release.tag_name
if (-not $tag) { throw "${Bin}: could not resolve a release version" }
$verNoV = $tag.TrimStart('v')

$asset = "${Bin}_${verNoV}_bnk-${Bnk}_windows_${arch}.zip"
$sums = "${Bin}_${verNoV}_checksums.txt"
$assetEntry = $release.assets | Where-Object { $_.name -eq $asset } | Select-Object -First 1
$sumsEntry = $release.assets | Where-Object { $_.name -eq $sums } | Select-Object -First 1

if (-not $assetEntry) {
  $have = $release.assets |
    ForEach-Object { if ($_.name -match "^${Bin}_[^_]+_bnk-([^_]+)_[^_]+_[^_]+\.(tar\.gz|zip)$") { $Matches[1] } } |
    Sort-Object -Unique
  if ($have) {
    throw "${Bin}: release $tag has no $asset; it has archives for BNK: $($have -join ', '). Set BNK_VERSION to one of those, or VERSION to a release that has BNK $Bnk."
  }
  throw "${Bin}: release $tag has no $asset, and no BNK archives at all."
}
if (-not $sumsEntry) { throw "${Bin}: release $tag has no $sums; refusing to install without checksum verification" }

# ---- download into a temp dir that is removed in finally ------------------------
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("$Bin-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
  Write-Host "Downloading $Bin $tag for BNK $Bnk (windows/$arch)..."
  $zip = Join-Path $tmp $asset
  Invoke-WebRequest -Uri $assetEntry.browser_download_url -OutFile $zip -UseBasicParsing
  $sumsPath = Join-Path $tmp $sums
  Invoke-WebRequest -Uri $sumsEntry.browser_download_url -OutFile $sumsPath -UseBasicParsing

  $want = $null
  foreach ($line in Get-Content -Path $sumsPath) {
    $f = $line.Trim() -split '\s+'
    if ($f.Count -ge 2 -and $f[1] -eq $asset) { $want = $f[0].ToLower(); break }
  }
  if (-not $want) { throw "${Bin}: $sums does not list $asset; refusing to install an unverified archive" }
  $got = (Get-FileHash -Path $zip -Algorithm SHA256).Hash.ToLower()
  if ($want -ne $got) { throw "${Bin}: checksum mismatch for $asset (want $want, got $got)" }
  Write-Host 'Checksum OK.'

  # extract + hand off to the binary's own installer
  $x = Join-Path $tmp 'x'
  Expand-Archive -Path $zip -DestinationPath $x -Force
  $exe = Join-Path $x "$Bin.exe"
  if (-not (Test-Path $exe)) { throw "${Bin}: $Bin.exe not found in $asset" }

  Write-Host "Installing via '$Bin self install'..."
  # Judge the native binary by its exit code, not by native-stderr text.
  $prev = $ErrorActionPreference
  $ErrorActionPreference = 'Continue'
  $extra = @()
  if ($InstallDir) { $extra += @('--dir', $InstallDir) }
  if ($InstallArgs) { $extra += $InstallArgs.Split(' ', [System.StringSplitOptions]::RemoveEmptyEntries) }
  & $exe self install --force @extra 2>&1 | ForEach-Object { Write-Host $_ }
  $code = $LASTEXITCODE
  $ErrorActionPreference = $prev
  if ($code -ne 0) { throw "$Bin self install failed (exit $code)" }
}
finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host "Done. Run '$Bin version' to confirm."
}
