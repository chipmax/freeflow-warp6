param(
  [switch]$NoStartup
)

$ErrorActionPreference = 'Stop'
$Repo = Split-Path -Parent $PSScriptRoot
$Config = Join-Path $Repo 'config.json'
$Example = Join-Path $Repo 'config.example.json'
$Exe = Join-Path $Repo 'freeflow-warp6.exe'

if (-not (Test-Path $Config)) {
  Copy-Item $Example $Config
  Write-Host "Created $Config"
}

# Build with native Go when available, otherwise with Go inside WSL.
$go = Get-Command go -ErrorAction SilentlyContinue
if ($go) {
  Push-Location $Repo
  try { & go build -o $Exe ./cmd/freeflow-warp6 } finally { Pop-Location }
} elseif (Get-Command wsl -ErrorAction SilentlyContinue) {
  $wslRepo = ($Repo -replace '\\', '/') -replace '^([A-Za-z]):', '/mnt/$1'
  $wslRepo = $wslRepo.ToLower()
  & wsl -e bash -lc "cd '$wslRepo' && GOOS=windows GOARCH=amd64 go build -o freeflow-warp6.exe ./cmd/freeflow-warp6"
} else {
  throw 'Go 1.24+ is required (native or inside WSL).'
}

# pi-freeflow settings. The relay URLs are loopback HTTP; ALLOW_UNSAFE_RELAY
# is required by pi-freeflow's URL validator. Bun keeps Zen's TLS fingerprint.
[Environment]::SetEnvironmentVariable('PI_FREEFLOW_ZEN_TRANSPORT', 'bun-sidecar', 'User')
[Environment]::SetEnvironmentVariable('PI_FREEFLOW_ALLOW_UNSAFE_RELAY', '1', 'User')
$env:PI_FREEFLOW_ZEN_TRANSPORT = 'bun-sidecar'
$env:PI_FREEFLOW_ALLOW_UNSAFE_RELAY = '1'

& (Join-Path $PSScriptRoot 'setup-freeflow.ps1')

if (-not $NoStartup) {
  $startup = [Environment]::GetFolderPath('Startup')
  $cmd = Join-Path $startup 'freeflow-warp6.cmd'
  @(
    '@echo off',
    "start ""freeflow-warp6"" /min ""$Exe"" -config ""$Config"""
  ) | Set-Content -Encoding ASCII $cmd
  Write-Host "Startup entry: $cmd"
}

Write-Host "Built: $Exe"
Write-Host 'Start it once; warp-state.json will be created with the WARP device keys.'
