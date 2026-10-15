#!/usr/bin/env python3
"""Sign with the selected certificate and strengthen its existing requirement."""
import os
import pathlib
import re
import subprocess
import sys


def run(*arguments):
    return subprocess.run(arguments, check=True, text=True, capture_output=True)


def sign(path, identifier):
    identity = os.environ.get("QSO_SIGN_IDENTITY", "")
    if not identity or identity == "-" or not re.fullmatch(r"local\.quota\.session\.guard(?:\.daemon|\.diagnostic)?", identifier):
        raise ValueError("必须显式选择现有证书和本项目组件身份")
    if path.is_symlink() or not path.exists():
        raise ValueError("签名目标不存在或为符号链接")
    # Local Apple Development builds stay offline (--timestamp=none). Developer ID
    # distribution needs a secure timestamp for notarization: QSO_SIGN_TIMESTAMP=1.
    stamp = os.environ.get("QSO_SIGN_TIMESTAMP", "0")
    if stamp not in {"0", "1"}:
        raise ValueError("QSO_SIGN_TIMESTAMP 只能为 0 或 1")
    timestamp = "--timestamp" if stamp == "1" else "--timestamp=none"
    arguments = ["/usr/bin/codesign", "--force", "--sign", identity, "--options", "runtime", timestamp, "--identifier", identifier]
    run(*arguments, str(path))
    result = run("/usr/bin/codesign", "-d", "--verbose=4", "-r-", str(path))
    detail = result.stdout + result.stderr
    team = re.search(r"^TeamIdentifier=([A-Z0-9]{10})$", detail, re.M)
    requirement = re.search(r"^designated => (.+)$", detail, re.M)
    if not team or not requirement or "Signature=adhoc" in detail:
        raise ValueError("证书团队或默认designated requirement无法核验")
    original = requirement.group(1)
    if "cdhash" in original or identifier not in original or "anchor" not in original or "certificate" not in original:
        raise ValueError("默认证书身份不是可核验的稳定身份；拒绝重写为宽松要求")
    # Keep every original predicate, including certificate pinning when present.
    # Appending OU prevents another team's certificate from satisfying the identity.
    team_predicate = f'certificate leaf[subject.OU] = "{team.group(1)}"'
    strengthened = 'designated => ' + original if team_predicate in original else f'designated => ({original}) and {team_predicate}'
    run(*arguments, "--requirements", "=" + strengthened, str(path))
    run("/usr/bin/codesign", "--verify", "--strict", str(path))
    print("组件证书签名及原要求＋团队约束通过：" + identifier)


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3:
            raise ValueError("需要签名目标与组件identifier")
        sign(pathlib.Path(sys.argv[1]), sys.argv[2])
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        # Do not print certificate subject names, keychain diagnostics or identity.
        if isinstance(error, subprocess.CalledProcessError):
            raise SystemExit("签名或要求验证失败，进程返回非零；未生成可分发产物")
        raise SystemExit(str(error))
