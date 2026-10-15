#!/usr/bin/env python3
"""Merge only this project's command hooks; preview unless --apply is explicit."""
import argparse
import copy
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time


EVENTS = {"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "PostModelSwitch", "Notification"}


def merge(existing, fragment, uninstall=False):
    result = copy.deepcopy(existing)
    if not isinstance(result, dict):
        raise ValueError("现有 settings 必须为 JSON 对象")
    if result.get("disableAllHooks") or result.get("allowManagedHooksOnly"):
        raise ValueError("现有策略限制 Hooks；安装器不会修改或绕过策略")
    hooks = result.setdefault("hooks", {})
    if not isinstance(hooks, dict):
        raise ValueError("现有 hooks 字段不是对象")
    for event, groups in fragment["hooks"].items():
        if event not in EVENTS or not isinstance(groups, list):
            raise ValueError("生成的事件配置无效")
        current = hooks.setdefault(event, [])
        if not isinstance(current, list):
            raise ValueError(f"现有 {event} 配置不是数组")
        commands = {h["command"] for group in groups for h in group["hooks"]}
        if uninstall:
            retained = []
            for group in current:
                if not isinstance(group, dict) or not isinstance(group.get("hooks"), list):
                    raise ValueError("现有 Hook 组无效")
                kept = [h for h in group["hooks"] if h.get("command") not in commands]
                if kept:
                    next_group = copy.deepcopy(group)
                    next_group["hooks"] = kept
                    retained.append(next_group)
            hooks[event] = retained
        else:
            for group in groups:
                if group not in current:
                    current.append(copy.deepcopy(group))
    return result


def atomic_replace(path, data, original):
    if path.is_symlink():
        raise ValueError("拒绝修改符号链接 settings")
    now = path.read_bytes() if path.exists() else None
    if now != original:
        raise ValueError("settings 在预览后已变化，拒绝覆盖")
    if original is not None:
        backup = path.with_name(path.name + ".qso-backup-" + str(time.time_ns()))
        fd = os.open(backup, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as out:
            out.write(original)
            out.flush()
            os.fsync(out.fileno())
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".qso-settings-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as out:
            os.fchmod(out.fileno(), 0o600)
            out.write(data)
            out.flush()
            os.fsync(out.fileno())
        # Compare again immediately before replacing to reduce lost-update races.
        if (path.read_bytes() if path.exists() else None) != original:
            raise ValueError("settings 同时被其他进程修改")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description="生成或显式合并 Code 生命周期 Hooks，不修改权限")
    parser.add_argument("--settings", type=Path, required=True, help="由用户明确选择的 settings 路径")
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--engine-version", required=True, help="Desktop 内嵌 Code 的真实版本")
    parser.add_argument("--apply", action="store_true", help="明确写入；默认只预览要增加的 Hooks")
    parser.add_argument("--uninstall", action="store_true")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    target = args.settings.expanduser().absolute()
    if target.is_symlink():
        raise ValueError("拒绝修改符号链接 settings")
    original = target.read_bytes() if target.exists() else None
    existing = json.loads(original) if original else {}
    command = [str(root / "bin/orchestrator"), "hooks-config", "--config", str(args.config.absolute()), "--engine-version", args.engine_version]
    fragment = json.loads(subprocess.check_output(command, cwd=root, text=True))
    result = merge(existing, fragment, args.uninstall)
    if not args.apply:
        # Do not print unrelated settings, which can contain user secrets.
        print(json.dumps(fragment, ensure_ascii=False, indent=2))
        print(f"目标：{target}\n现有设置将保留。未写入；应用需要显式 --apply。")
        return
    data = (json.dumps(result, ensure_ascii=False, indent=2) + "\n").encode()
    atomic_replace(target, data, original)
    print(f"已{'移除本项目' if args.uninstall else '合并本项目'} Hooks：{target}；未修改任何权限或管理员策略。")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as exc:
        raise SystemExit(f"安装失败：{exc}")
