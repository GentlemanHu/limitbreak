#!/bin/bash
# LimitBreak installer for macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/GentlemanHu/limitbreak/main/scripts/install.sh | bash
#
# What it does:
#   1. Looks up the latest GitHub release (unauthenticated API) or LIMITBREAK_VERSION.
#   2. Downloads LimitBreak-<version>-universal.zip and SHA256SUMS, verifies the checksum.
#   3. Quits a running LimitBreak (by bundle id), backs up the previous app, installs the new one.
#   4. Removes the quarantine attribute and opens the app.
#
# Environment overrides:
#   LIMITBREAK_REPO         GitHub "owner/repo" (default GentlemanHu/limitbreak)
#   LIMITBREAK_VERSION      install a specific version, e.g. 1.0.0 (default: latest release)
#   LIMITBREAK_INSTALL_DIR  target directory (default /Applications)
#   LIMITBREAK_BACKUP_DIR   where previous versions are kept (default ~/.limitbreak/backups)
#   LIMITBREAK_NO_OPEN=1    do not launch the app after installing
set -euo pipefail

REPO="${LIMITBREAK_REPO:-GentlemanHu/limitbreak}"
BUNDLE_ID="local.quota.session.guard"
APP_NAME="LimitBreak.app"
INSTALL_DIR="${LIMITBREAK_INSTALL_DIR:-/Applications}"
BACKUP_ROOT="${LIMITBREAK_BACKUP_DIR:-$HOME/.limitbreak/backups}"

say() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[[ "$(uname -s)" == Darwin ]] || die "LimitBreak is a macOS app; refusing to run on $(uname -s)."
[[ "$(id -u)" != 0 ]] || die "Do not run this installer as root; it asks for sudo only if $INSTALL_DIR is not writable."
OS_MAJOR="$(sw_vers -productVersion | cut -d. -f1)"
[[ "$OS_MAJOR" =~ ^[0-9]+$ && "$OS_MAJOR" -ge 14 ]] || die "LimitBreak requires macOS 14 (Sonoma) or later."
[[ "$REPO" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || die "LIMITBREAK_REPO must look like owner/repo."
for tool in curl shasum ditto plutil xattr codesign; do
  command -v "$tool" >/dev/null 2>&1 || die "missing required tool: $tool"
done

WORK="$(mktemp -d "${TMPDIR:-/tmp}/limitbreak-install.XXXXXX")"
STAGED=""
cleanup() {
  if [[ -n "$STAGED" && -e "$STAGED" ]]; then ${SUDO:-} rm -rf "$STAGED" || true; fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# 1. Resolve the release tag.
if [[ -n "${LIMITBREAK_VERSION:-}" ]]; then
  TAG="v${LIMITBREAK_VERSION#v}"
else
  say "Looking up the latest release of $REPO"
  curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/$REPO/releases/latest" -o "$WORK/release.json" \
    || die "could not query GitHub releases (rate limited or no published release yet)."
  TAG="$(plutil -extract tag_name raw -o - "$WORK/release.json" 2>/dev/null)" || die "release metadata has no tag_name."
fi
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "unexpected release tag: $TAG"
VERSION="${TAG#v}"
ZIP="LimitBreak-${VERSION}-universal.zip"
BASE="https://github.com/$REPO/releases/download/$TAG"

# 2. Download and verify.
say "Downloading $ZIP"
curl -fL --progress-bar "$BASE/$ZIP" -o "$WORK/$ZIP" || die "download failed: $BASE/$ZIP"
curl -fsSL "$BASE/SHA256SUMS" -o "$WORK/SHA256SUMS" || die "download failed: $BASE/SHA256SUMS"
EXPECTED="$(awk -v name="$ZIP" '$2 == name || $2 == "*" name { print $1 }' "$WORK/SHA256SUMS")"
[[ "$EXPECTED" =~ ^[0-9a-f]{64}$ ]] || die "SHA256SUMS has no entry for $ZIP."
ACTUAL="$(shasum -a 256 "$WORK/$ZIP" | awk '{ print $1 }')"
[[ "$ACTUAL" == "$EXPECTED" ]] || die "checksum mismatch for $ZIP (expected $EXPECTED, got $ACTUAL)."
say "Checksum verified"

mkdir "$WORK/extract"
ditto -x -k "$WORK/$ZIP" "$WORK/extract"
NEW_APP="$WORK/extract/$APP_NAME"
[[ -d "$NEW_APP" && ! -L "$NEW_APP" ]] || die "archive does not contain $APP_NAME."
[[ "$(plutil -extract CFBundleIdentifier raw -o - "$NEW_APP/Contents/Info.plist")" == "$BUNDLE_ID" ]] || die "unexpected bundle identifier in archive."
codesign --verify --deep --strict "$NEW_APP" || die "code signature verification failed."
ADHOC="$(plutil -extract QSOUnstableAdHocIdentity raw -o - "$NEW_APP/Contents/Info.plist" 2>/dev/null || echo false)"

# 3. Choose privileges for the install directory.
SUDO=""
mkdir -p "$INSTALL_DIR" 2>/dev/null || true
if [[ ! -w "$INSTALL_DIR" ]]; then
  say "$INSTALL_DIR is not writable by $(id -un); sudo is needed only to move the app into place."
  say "(Alternatively re-run with LIMITBREAK_INSTALL_DIR=\$HOME/Applications.)"
  SUDO="sudo"
fi

# Quit a running instance (any app with this bundle id, including the legacy 额度守护.app).
running() { pgrep -f '/Contents/MacOS/(LimitBreak|QuotaGuard)$' >/dev/null 2>&1; }
if running; then
  say "Quitting the running LimitBreak"
  osascript -e "tell application id \"$BUNDLE_ID\" to quit" >/dev/null 2>&1 || true
  for _ in $(seq 1 30); do running || break; sleep 0.5; done
  running && die "LimitBreak is still running; quit it from the menu bar and run the installer again."
fi

# 4. Stage next to the target, back up the previous version, then swap.
TARGET="$INSTALL_DIR/$APP_NAME"
STAGED="$INSTALL_DIR/.LimitBreak.app.installing.$$"
$SUDO ditto "$NEW_APP" "$STAGED"
BACKUP=""
if [[ -e "$TARGET" || -L "$TARGET" ]]; then
  OLD_VERSION="$(plutil -extract CFBundleShortVersionString raw -o - "$TARGET/Contents/Info.plist" 2>/dev/null || echo unknown)"
  BACKUP_DIR="$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S)-$OLD_VERSION"
  mkdir -p "$BACKUP_DIR"
  BACKUP="$BACKUP_DIR/$APP_NAME"
  $SUDO mv "$TARGET" "$BACKUP"
  if [[ -n "$SUDO" ]]; then sudo chown -R "$(id -u):$(id -g)" "$BACKUP_DIR" || true; fi
  say "Previous version $OLD_VERSION moved to $BACKUP"
fi
if ! $SUDO mv "$STAGED" "$TARGET"; then
  if [[ -n "$BACKUP" ]]; then $SUDO mv "$BACKUP" "$TARGET" || true; fi
  die "could not move the new app into $INSTALL_DIR; the previous version was restored."
fi
STAGED=""
# curl downloads are not quarantined, but clear it anyway in case the archive was copied in.
$SUDO xattr -dr com.apple.quarantine "$TARGET" 2>/dev/null || true
say "Installed LimitBreak $VERSION to $TARGET"

if [[ -d "/Applications/额度守护.app" ]]; then
  warn "an older copy /Applications/额度守护.app (same bundle id) is still installed; remove it to avoid launching the wrong version."
fi
if [[ "$ADHOC" == true ]]; then
  warn "this build is ad-hoc signed (not notarized). Its code identity changes with every build, so macOS"
  warn "will ask you to grant Accessibility again: System Settings > Privacy & Security > Accessibility,"
  warn "remove the old LimitBreak entry with '-', then add $TARGET with '+'."
fi
if [[ "${LIMITBREAK_NO_OPEN:-0}" != 1 ]]; then
  open "$TARGET" || warn "could not launch automatically; open $TARGET from Finder."
fi
