# 一键编译 5 个产物：linux amd64/arm64、windows amd64、macOS amd64/arm64
# 用法: powershell -ExecutionPolicy Bypass -File build.ps1
$ErrorActionPreference = "Stop"
Set-Location -Path $PSScriptRoot

$env:CGO_ENABLED = "0"
$LDFLAGS = "-s -w"
$OutDir = "dist"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

function Build([string]$goos, [string]$goarch, [string]$name) {
  Write-Host "==> building $name"
  $env:GOOS = $goos
  $env:GOARCH = $goarch
  $target = Join-Path $OutDir $name
  go build -trimpath -ldflags $LDFLAGS -o $target .
  if ($LASTEXITCODE -ne 0) { throw "build failed: $name" }
  if (-not (Test-Path $target)) { throw "output missing: $target" }
}

Build "linux"   "amd64" "gdkm-qy-campus-auth-linux-amd64"
Build "linux"   "arm64" "gdkm-qy-campus-auth-linux-arm64"
Build "windows" "amd64" "gdkm-qy-campus-auth-windows-amd64.exe"
Build "darwin"  "amd64" "gdkm-qy-campus-auth-darwin-amd64"
Build "darwin"  "arm64" "gdkm-qy-campus-auth-darwin-arm64"

Remove-Item Env:GOOS -ErrorAction SilentlyContinue
Remove-Item Env:GOARCH -ErrorAction SilentlyContinue

Write-Host "全部产物已输出至 $OutDir/"
