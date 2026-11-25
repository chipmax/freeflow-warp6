param(
  [string]$Config = (Join-Path (Split-Path -Parent $PSScriptRoot) 'config.json')
)

$ErrorActionPreference = 'Stop'
$cfg = Get-Content $Config -Raw | ConvertFrom-Json
$base = [int]$cfg.listen_base
$count = [int]$cfg.relay_count
$control = [int]$cfg.control_port

$relays = for ($i = 0; $i -lt $count; $i++) {
  [pscustomobject]@{
    url   = "http://127.0.0.1:$($base + $i)"
    label = "warp6-$($i + 1)"
  }
}

$statePath = Join-Path $HOME '.pi/agent/pi-freeflow-relay-state.json'
$dir = Split-Path -Parent $statePath
if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force -Path $dir | Out-Null }

$state = [pscustomobject]@{
  enabled    = $true
  mode       = 'spread'
  url        = $relays[0].url
  relays     = $relays
  rotateBase = "http://127.0.0.1:$control"
  hideWidget = $false
  autoUpdate = $false
}

$state | ConvertTo-Json -Depth 6 | Set-Content -Encoding UTF8 $statePath
Write-Host "Wrote $statePath with $count relay(s), rotateBase http://127.0.0.1:$control"
