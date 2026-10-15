#!/usr/bin/env bash
# 一键交叉编译 5 个产物：linux amd64/arm64、windows amd64、macOS amd64/arm64
set -euo pipefail
cd "$(dirname "$0")"

export CGO_ENABLED=0
LDFLAGS="-s -w"
OUT=dist
mkdir -p "$OUT"

build() {
  local goos="$1" goarch="$2" out="$3"
  echo "==> building ${out}"
  GOOS="$goos" GOARCH="$goarch" go build -trimpath -ldflags "$LDFLAGS" -o "${OUT}/${out}" .
}

build linux   amd64 gdkm-qy-campus-auth-linux-amd64
build linux   arm64 gdkm-qy-campus-auth-linux-arm64
build windows amd64 gdkm-qy-campus-auth-windows-amd64.exe
build darwin  amd64 gdkm-qy-campus-auth-darwin-amd64
build darwin  arm64 gdkm-qy-campus-auth-darwin-arm64

echo "全部产物已输出至 ${OUT}/"
