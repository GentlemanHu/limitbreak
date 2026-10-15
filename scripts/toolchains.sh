#!/bin/bash
# Sourced by build scripts; use existing tools without personal installation paths.
qso_tools() {
  GO="${QSO_GO:-$(command -v go || true)}"
  if [[ -z "$GO" ]]; then
    printf '%s\n' '需要已有 Go。使用 PATH 或 QSO_GO 指定；不会自动安装工具链。' >&2
    return 1
  fi
  export GOCACHE="${GOCACHE:-$ROOT/.build/go-cache}"
  export GOPATH="${GOPATH:-$ROOT/.build/go-path}"
}
qso_macos_tools() {
  [[ "$(uname -s)" == Darwin ]] || { printf '%s\n' '原生应用必须在 macOS 上构建。' >&2; return 1; }
  SWIFTC="${QSO_SWIFTC:-$(command -v swiftc || true)}"
  SDK="${QSO_MACOS_SDK:-${SDKROOT:-}}"
  if [[ -z "$SDK" ]]; then
    local developer="${DEVELOPER_DIR:-$(/usr/bin/xcode-select -p)}"
    for candidate in "$developer/SDKs/MacOSX.sdk" "$developer/Platforms/MacOSX.platform/Developer/SDKs/MacOSX.sdk"; do
      if [[ -d "$candidate" ]]; then SDK="$candidate"; break; fi
    done
  fi
  [[ -n "$SWIFTC" && -d "$SDK" ]] || { printf '%s\n' '需要已有 Swift 和 macOS SDK。可通过 QSO_SWIFTC、QSO_MACOS_SDK 指定。' >&2; return 1; }
  ARCH="${QSO_ARCH:-$(uname -m)}"
  case "$ARCH" in arm64) GOARCH=arm64;; x86_64) GOARCH=amd64;; *) printf '%s\n' '仅支持 arm64 或 x86_64 构建目标。' >&2; return 1;; esac
  export MACOSX_DEPLOYMENT_TARGET=14.0
  TARGET="$ARCH-apple-macos14.0"
}
