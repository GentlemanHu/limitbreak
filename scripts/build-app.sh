#!/bin/bash
# Build LimitBreak.app. Bundle identifiers stay local.quota.session.guard[.daemon|.diagnostic]
# so existing Accessibility (TCC) grants and user data keep working across the rename.
#
# Environment:
#   QSO_SIGN_IDENTITY   existing local signing identity (required unless QSO_ALLOW_ADHOC=1)
#   QSO_ALLOW_ADHOC=1   explicit ad-hoc signing (unstable permission identity)
#   QSO_SIGN_TIMESTAMP=1  request a secure timestamp (Developer ID / notarization); default none
#   QSO_UNIVERSAL=1     build arm64 + x86_64 universal binaries; default is the native (or QSO_ARCH) slice
#   QSO_APP_OUTPUT      output path, must end in LimitBreak.app and must not exist
#   QSO_CREATE_ZIP=1    also create LimitBreak-<version>-<arch|universal>.zip next to the app
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "$ROOT/scripts/toolchains.sh"
qso_tools
qso_macos_tools
VERSION="$(<"$ROOT/VERSION")"
[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { printf '%s\n' 'VERSION 格式无效。' >&2; exit 1; }
APP_NAME='LimitBreak.app'
EXECUTABLE='LimitBreak'
APP="${QSO_APP_OUTPUT:-$ROOT/dist/$APP_NAME}"
[[ "$(basename "$APP")" == "$APP_NAME" ]] || { printf '%s\n' "安装包名必须固定为 $APP_NAME；版本仅进入元数据与 ZIP 名。" >&2; exit 1; }
[[ ! -e "$APP" && ! -L "$APP" ]] || { printf '不覆盖既有产物：%s\n请指定新目录下的 %s。\n' "$APP" "$APP_NAME" >&2; exit 1; }
case "${QSO_UNIVERSAL:-0}" in
  1) ARCHES=(arm64 x86_64); VERIFY_ARCH=universal;;
  0) ARCHES=("$ARCH"); VERIFY_ARCH="$ARCH";;
  *) printf '%s\n' 'QSO_UNIVERSAL 只能为 0 或 1。' >&2; exit 1;;
esac
case "${QSO_SIGN_TIMESTAMP:-0}" in
  0|1) ;;
  *) printf '%s\n' 'QSO_SIGN_TIMESTAMP 只能为 0 或 1。' >&2; exit 1;;
esac
IDENTITY="${QSO_SIGN_IDENTITY:-}"
ADHOC=0
if [[ -z "$IDENTITY" || "$IDENTITY" == '-' ]]; then
  [[ "${QSO_ALLOW_ADHOC:-}" == 1 ]] || { printf '%s\n' '必须通过 QSO_SIGN_IDENTITY 显式选择本机现有签名证书；不会静默 ad-hoc。' >&2; exit 1; }
  IDENTITY='-'
  ADHOC=1
  printf '%s\n' '警告：显式 ad-hoc 构建，不提供稳定辅助功能权限身份；不适合权限验收或稳定升级。' >&2
fi
mkdir -p "$ROOT/.build" "$(dirname "$APP")"
: "${TMPDIR:?TMPDIR 必须已设置}"
STAGE="$(mktemp -d "${TMPDIR%/}/qso-app-stage.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT
BUNDLE="$STAGE/$APP_NAME"
mkdir -p "$BUNDLE/Contents/MacOS" "$BUNDLE/Contents/Resources/bin"
SHARED=()
for SOURCE in "$ROOT"/bridge/*.swift; do
  [[ "$(basename "$SOURCE")" == main.swift ]] || SHARED+=("$SOURCE")
done
BASE_CGO_CFLAGS="${CGO_CFLAGS:-}"
BASE_CGO_LDFLAGS="${CGO_LDFLAGS:-}"

# Build one architecture slice of all three executables into $2.
build_slice() {
  local arch="$1" out="$2" goarch target
  case "$arch" in arm64) goarch=arm64;; x86_64) goarch=amd64;; *) printf '%s\n' '仅支持 arm64 或 x86_64 构建目标。' >&2; return 1;; esac
  target="$arch-apple-macos14.0"
  mkdir -p "$out"
  GOOS=darwin GOARCH="$goarch" CGO_ENABLED=1 \
    CGO_CFLAGS="$BASE_CGO_CFLAGS -arch $arch -mmacosx-version-min=14.0" CGO_LDFLAGS="$BASE_CGO_LDFLAGS -arch $arch -mmacosx-version-min=14.0" \
    "$GO" -C "$ROOT" build -trimpath -o "$out/orchestrator" ./cmd/orchestrator
  "$SWIFTC" -sdk "$SDK" -target "$target" -O -module-cache-path "$ROOT/.build/swift-cache" -o "$out/desktop-bridge" "$ROOT"/bridge/*.swift -framework AppKit -framework ApplicationServices -framework IOKit -framework Security -framework CryptoKit
  "$SWIFTC" -sdk "$SDK" -target "$target" -parse-as-library -O -warnings-as-errors -module-cache-path "$ROOT/.build/swift-cache" \
    "$ROOT"/tray/*.swift "${SHARED[@]}" -framework AppKit -framework SwiftUI -framework ServiceManagement -framework ApplicationServices -framework IOKit -framework Security -framework CryptoKit \
    -o "$out/$EXECUTABLE"
}

# Place one component: the single slice as-is, or a lipo-merged universal binary.
assemble() {
  local component="$1" destination="$2" arch
  local -a inputs
  inputs=()
  if [[ "${#ARCHES[@]}" == 1 ]]; then
    mv "$STAGE/slices/${ARCHES[0]}/$component" "$destination"
    return
  fi
  for arch in "${ARCHES[@]}"; do inputs+=("$STAGE/slices/$arch/$component"); done
  /usr/bin/lipo -create "${inputs[@]}" -output "$destination"
}

for SLICE_ARCH in "${ARCHES[@]}"; do
  build_slice "$SLICE_ARCH" "$STAGE/slices/$SLICE_ARCH"
done
assemble orchestrator "$BUNDLE/Contents/Resources/bin/orchestrator"
assemble desktop-bridge "$BUNDLE/Contents/Resources/bin/desktop-bridge"
assemble "$EXECUTABLE" "$BUNDLE/Contents/MacOS/$EXECUTABLE"
"$SWIFTC" -sdk "$SDK" -O -module-cache-path "$ROOT/.build/swift-cache" "$ROOT/scripts/MakeIcon.swift" -o "$STAGE/make-icon"
"$STAGE/make-icon" "$BUNDLE/Contents/Resources/AppIcon.icns"
# Provider icons are optional resources; they must be copied before the bundle is sealed.
if [[ -d "$ROOT/tray/Resources/ProviderIcons" ]]; then
  cp -R "$ROOT/tray/Resources/ProviderIcons" "$BUNDLE/Contents/Resources/"
  find "$BUNDLE/Contents/Resources/ProviderIcons" -name .DS_Store -type f -delete
  # The icon directory holds images only; their attribution ships beside it.
  if [[ -f "$BUNDLE/Contents/Resources/ProviderIcons/NOTICE.md" ]]; then
    mv "$BUNDLE/Contents/Resources/ProviderIcons/NOTICE.md" "$BUNDLE/Contents/Resources/ProviderIcons-NOTICE.md"
  fi
fi
/usr/bin/python3 -I - "$BUNDLE" "$VERSION" "$ADHOC" "$EXECUTABLE" <<'PY'
import pathlib, plistlib, sys
app = pathlib.Path(sys.argv[1])
info = {
    'CFBundleIdentifier': 'local.quota.session.guard',
    'CFBundleName': 'LimitBreak', 'CFBundleDisplayName': 'LimitBreak',
    'CFBundleExecutable': sys.argv[4], 'CFBundlePackageType': 'APPL',
    'CFBundleShortVersionString': sys.argv[2], 'CFBundleVersion': sys.argv[2],
    'CFBundleIconFile': 'AppIcon.icns', 'LSUIElement': True,
    # The interface text is authored in Simplified Chinese; English is declared so
    # system-provided UI follows the user's language.
    'CFBundleDevelopmentRegion': 'zh-Hans',
    'CFBundleLocalizations': ['zh-Hans', 'en'],
    'CFBundleAllowMixedLocalizations': True,
    'LSMinimumSystemVersion': '14.0', 'NSHighResolutionCapable': True,
    'QSOUnstableAdHocIdentity': sys.argv[3] == '1',
    'NSAppTransportSecurity': {'NSAllowsLocalNetworking': True},
}
(app / 'Contents/Info.plist').write_bytes(plistlib.dumps(info))
PY
SIGN_ARGS=(--force --sign "$IDENTITY" --options runtime)
if [[ "$ADHOC" == 0 ]]; then
  if [[ "${QSO_SIGN_TIMESTAMP:-0}" == 1 ]]; then SIGN_ARGS+=(--timestamp); else SIGN_ARGS+=(--timestamp=none); fi
fi
if [[ "$ADHOC" == 1 ]]; then
  /usr/bin/codesign "${SIGN_ARGS[@]}" --identifier local.quota.session.guard.diagnostic "$BUNDLE/Contents/Resources/bin/desktop-bridge"
  /usr/bin/codesign "${SIGN_ARGS[@]}" --identifier local.quota.session.guard.daemon "$BUNDLE/Contents/Resources/bin/orchestrator"
  /usr/bin/codesign "${SIGN_ARGS[@]}" --identifier local.quota.session.guard "$BUNDLE"
else
  export QSO_SIGN_IDENTITY="$IDENTITY"
  export QSO_SIGN_TIMESTAMP="${QSO_SIGN_TIMESTAMP:-0}"
  /usr/bin/python3 -I "$ROOT/scripts/stable_sign.py" "$BUNDLE/Contents/Resources/bin/desktop-bridge" local.quota.session.guard.diagnostic
  /usr/bin/python3 -I "$ROOT/scripts/stable_sign.py" "$BUNDLE/Contents/Resources/bin/orchestrator" local.quota.session.guard.daemon
  /usr/bin/python3 -I "$ROOT/scripts/stable_sign.py" "$BUNDLE" local.quota.session.guard
fi
# macOS /bin/bash 3.2 treats an empty array expansion as unset under set -u.
if [[ "$ADHOC" == 1 ]]; then
  /usr/bin/python3 -I "$ROOT/scripts/verify_app.py" "$BUNDLE" "$VERIFY_ARCH" --allow-adhoc
else
  /usr/bin/python3 -I "$ROOT/scripts/verify_app.py" "$BUNDLE" "$VERIFY_ARCH"
fi
mv "$BUNDLE" "$APP"
if [[ "${QSO_CREATE_ZIP:-0}" == 1 ]]; then
  ZIP_NAME="LimitBreak-${VERSION}-${VERIFY_ARCH}.zip"
  ZIP="$(dirname "$APP")/$ZIP_NAME"
  [[ ! -e "$ZIP" && ! -L "$ZIP" ]] || { printf '不覆盖既有 ZIP：%s\n' "$ZIP" >&2; exit 1; }
  /usr/bin/python3 -I "$ROOT/scripts/archive_app.py" "$APP" "$STAGE/$ZIP_NAME"
  mv "$STAGE/$ZIP_NAME" "$ZIP"
fi
printf '已生成：%s（%s）\n本机证书签名不等于 Developer ID 公证；分发前需另行公证。\n' "$APP" "$VERIFY_ARCH"
