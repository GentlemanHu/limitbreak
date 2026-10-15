#!/bin/bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"
source "$ROOT/scripts/toolchains.sh"
qso_tools
"$GO" vet ./...
"$GO" test -race -count=1 ./...
/usr/bin/python3 -I -m unittest discover -s scripts -p '*_test.py'
if [[ "$(uname -s)" == Darwin ]]; then
  qso_macos_tools
  mkdir -p bin .build
  "$SWIFTC" -sdk "$SDK" -target "$TARGET" -O -module-cache-path "$ROOT/.build/swift-cache" -o bin/desktop-bridge bridge/*.swift -framework AppKit -framework ApplicationServices
  SHARED_BRIDGE=()
  for path in "$ROOT"/bridge/*.swift; do
    if [[ "$(basename "$path")" != main.swift ]]; then SHARED_BRIDGE+=("$path"); fi
  done
  "$SWIFTC" -sdk "$SDK" -target "$TARGET" -parse-as-library -O -warnings-as-errors -module-cache-path "$ROOT/.build/swift-cache" -o bin/quota-workbench tray/*.swift "${SHARED_BRIDGE[@]}" -framework AppKit -framework SwiftUI -framework ServiceManagement -framework ApplicationServices -framework IOKit -framework Security -framework CryptoKit
  "$SWIFTC" -sdk "$SDK" -target "$TARGET" -parse-as-library -O -module-cache-path "$ROOT/.build/swift-cache" -o .build/native-model-tests tray/TrayModels.swift tray/GuardAdapterModels.swift scripts/TrayModelsTest.swift
  .build/native-model-tests
  "$SWIFTC" -sdk "$SDK" -target "$TARGET" -parse-as-library -O -module-cache-path "$ROOT/.build/swift-cache" -o .build/private-storage-tests tray/GuardPrivateStorage.swift scripts/PrivateStorageTest.swift -framework Security
  .build/private-storage-tests
  "$SWIFTC" -sdk "$SDK" -target "$TARGET" -parse-as-library -O -warnings-as-errors -module-cache-path "$ROOT/.build/swift-cache" -o .build/native-contract-tests tray/TrayModels.swift tray/GuardAdapterModels.swift tray/GuardPermissionModels.swift tray/GuardRuntimeFailure.swift tray/GuardPolicyModels.swift tray/GuardControlModels.swift tray/GuardScopedProfile.swift tray/GuardHookPlan.swift tray/TraySessionPatch.swift tray/DesktopIPCModels.swift bridge/BridgeTypes.swift scripts/NativeContractsTest.swift -framework CryptoKit
  .build/native-contract-tests
fi
printf '%s\n' '自动化验证通过；未向真实 Claude 会话发送任何消息。原生编译不等于桌面交互验收。'
