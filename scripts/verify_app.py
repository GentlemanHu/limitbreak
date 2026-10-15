#!/usr/bin/env python3
"""Read-only package checks. Never launches the app or reads runtime credentials."""
import argparse
import pathlib
import plistlib
import re
import subprocess

APP_NAME = "LimitBreak.app"
EXECUTABLE = "Contents/MacOS/LimitBreak"
UNIVERSAL = ["arm64", "x86_64"]
PROVIDER_ICON_SUFFIXES = {".png", ".pdf", ".svg", ".icns", ".json"}
# Bundle identifiers intentionally predate the LimitBreak name: TCC grants and data depend on them.
IDS = {
    EXECUTABLE: "local.quota.session.guard",
    "Contents/Resources/bin/orchestrator": "local.quota.session.guard.daemon",
    "Contents/Resources/bin/desktop-bridge": "local.quota.session.guard.diagnostic",
}
PRIVATE = {
    "config.json", "management-key", "management-token", "desktop-token", "desktop.token",
    "desktop.sock", "app.lock", ".env", "token", "token.json", "credentials.json",
    "api-key", "api_key", "auth.json", "keychain-export", "daemon.stderr",
}


def command(*args):
    result = subprocess.run(args, check=True, text=True, capture_output=True)
    return result.stdout + result.stderr


def fail(message):
    raise SystemExit(message)


def check_signature(path, identifier, allow_adhoc):
    command("/usr/bin/codesign", "--verify", "--strict", str(path))
    detail = command("/usr/bin/codesign", "-d", "--verbose=4", "-r-", str(path))
    match = re.search(r"^Identifier=(.+)$", detail, re.M)
    if not match or match.group(1) != identifier:
        fail("签名组件 identifier 不匹配：" + str(path))
    adhoc = "Signature=adhoc" in detail
    if adhoc:
        if not allow_adhoc:
            fail("拒绝默认 ad-hoc 签名；必须显式 --allow-adhoc")
        # An ad-hoc signature only has the implicit cdhash requirement, which
        # codesign prints as a comment ("# designated => cdhash ...").
        if not re.search(r"^(# )?designated => ", detail, re.M):
            fail("缺少 designated requirement：" + str(path))
        return None
    designated = re.search(r"^designated => (.+)$", detail, re.M)
    if not designated:
        fail("缺少 designated requirement：" + str(path))
    requirement = designated.group(1)
    team_match = re.search(r"^TeamIdentifier=(.+)$", detail, re.M)
    team = team_match.group(1) if team_match else None
    if not team or team == "not set":
        fail("稳定签名缺少 TeamIdentifier：" + str(path))
    # A release-independent identity must be anchored to a certificate, not a binary hash.
    if "cdhash" in requirement or not all(part in requirement for part in ("identifier", "anchor", "certificate")):
        fail("designated requirement 不是稳定证书身份：" + str(path))
    if identifier not in requirement or team not in requirement:
        fail("designated requirement 未绑定组件 identifier 与团队：" + str(path))
    return team


def check_binary(path, architecture):
    arches = sorted(command("/usr/bin/lipo", "-archs", str(path)).split())
    expected = UNIVERSAL if architecture == "universal" else [architecture]
    if arches != expected:
        fail("二进制架构不一致：" + str(path))
    # Universal binaries: inspect every slice's load commands, not just the host slice.
    loads = command("/usr/bin/otool", "-arch", "all", "-l", str(path)) if len(expected) > 1 else command("/usr/bin/otool", "-l", str(path))
    if len(expected) > 1 and len(re.findall(r"\bminos\s+[0-9.]+|LC_VERSION_MIN_MACOSX", loads)) < len(expected):
        fail("通用二进制未能核验每个架构的最低系统版本：" + str(path))
    minimums = re.findall(r"\bminos\s+([0-9.]+)", loads)
    minimums += re.findall(r"cmd LC_VERSION_MIN_MACOSX\s+cmdsize \d+\s+version ([0-9.]+)", loads)
    if not minimums or any(tuple(int(v) for v in item.split("."))[:2] != (14, 0) for item in minimums):
        fail("Mach-O 最低系统版本必须为 macOS 14.0：" + str(path))
    platforms = re.findall(r"\bplatform\s+(\S+)", loads)
    if platforms and any(value not in {"1", "macos", "MACOS"} for value in platforms):
        fail("Mach-O 平台不是 macOS：" + str(path))


def verify(app, architecture, version, allow_adhoc=False):
    if app.is_symlink() or app.name != APP_NAME or not app.is_dir():
        fail("应用必须为非符号链接、固定名称 " + APP_NAME)
    info = plistlib.loads((app / "Contents/Info.plist").read_bytes())
    expected = {
        "CFBundleIdentifier": "local.quota.session.guard", "CFBundleName": "LimitBreak",
        "CFBundleDisplayName": "LimitBreak", "CFBundleExecutable": "LimitBreak",
        "CFBundleShortVersionString": version, "CFBundleVersion": version,
        "LSUIElement": True, "LSMinimumSystemVersion": "14.0",
    }
    if any(info.get(key) != value for key, value in expected.items()):
        fail("应用 bundle 身份、版本或最低系统声明不匹配")
    required = [*IDS, "Contents/Resources/AppIcon.icns"]
    for name in required:
        if not (app / name).is_file():
            fail("应用组件缺失：" + name)
    for path in app.rglob("*"):
        relative = path.relative_to(app)
        if path.is_symlink():
            fail("应用中存在未预期符号链接：" + str(relative))
        if not path.is_file():
            if path.exists() and not path.is_dir():
                fail("应用中混入 socket 或特殊文件：" + str(relative))
            continue
        lower = path.name.lower()
        if lower in PRIVATE or lower.startswith(".env") or lower.endswith((".sqlite", ".db", ".sqlite-wal", ".sqlite-shm", ".sock", ".pem", ".p12", ".key", ".log")):
            fail("应用混入私有配置、凭据或运行文件：" + str(relative))
        if any(part.lower() in {".git", "node_modules", "traces", "runtime", "private-data"} for part in relative.parts):
            fail("应用混入开发或运行目录：" + str(relative))
        # Optional provider icon resources may only contain static image/asset files.
        if relative.parts[:3] == ("Contents", "Resources", "ProviderIcons") and path.suffix.lower() not in PROVIDER_ICON_SUFFIXES:
            fail("ProviderIcons 中存在非图片资源：" + str(relative))
    teams = []
    for name, identifier in IDS.items():
        path = app if name == EXECUTABLE else app / name
        teams.append(check_signature(path, identifier, allow_adhoc))
        check_binary(app / name, architecture)
    if len(set(teams)) != 1:
        fail("主 App、daemon、诊断 helper 签名团队不一致（或混用 ad-hoc）")
    adhoc = teams[0] is None
    if info.get("QSOUnstableAdHocIdentity") is not adhoc:
        fail("ad-hoc 权限不稳定标记与实际签名不一致")
    command("/usr/bin/codesign", "--verify", "--deep", "--strict", str(app))
    print(f"版本 {version}；固定 {APP_NAME}；macOS 14.0；{architecture}；组件与私有文件检查通过。")
    print("显式 ad-hoc：不具备稳定权限身份。" if adhoc else "三组件稳定证书身份及团队检查通过；不代表 Developer ID 公证。")


def main():
    root = pathlib.Path(__file__).resolve().parent.parent
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("app", nargs="?", type=pathlib.Path, default=root / "dist" / APP_NAME)
    parser.add_argument("architecture", nargs="?", choices=["arm64", "x86_64", "universal"], default="arm64")
    parser.add_argument("--allow-adhoc", action="store_true")
    args = parser.parse_args()
    version = (root / "VERSION").read_text().strip()
    verify(args.app, args.architecture, version, args.allow_adhoc)
    for directory in ("tray", "bridge", "scripts"):
        for path in (root / directory).rglob("*.swift"):
            count = len(path.read_text().splitlines())
            limit = 2000 if directory == "scripts" else 500
            if count > limit:
                fail(f"文件长度超限：{path.relative_to(root)} {count}/{limit}")


if __name__ == "__main__":
    main()
