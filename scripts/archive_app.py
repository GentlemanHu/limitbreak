#!/usr/bin/env python3
"""Create a standard ZIP of LimitBreak.app with POSIX executable permissions."""
import pathlib
import stat
import sys
import zipfile

APP_NAME = "LimitBreak.app"
EXECUTABLE = APP_NAME + "/Contents/MacOS/LimitBreak"


def archive(app, output):
    if app.name != APP_NAME or app.is_symlink() or not app.is_dir():
        raise ValueError("只能归档固定名称的自有应用目录 " + APP_NAME)
    if output.exists() or output.is_symlink():
        raise ValueError("拒绝覆盖既有ZIP")
    entries = [app, *sorted(app.rglob("*"))]
    for path in entries:
        if path.is_symlink() or not (path.is_dir() or path.is_file()):
            raise ValueError("归档中不接受符号链接或特殊文件")
    with zipfile.ZipFile(output, "x", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as package:
        for path in entries:
            # zipfile sets the UTF-8 flag for any non-ASCII entry name; no encoding guessing.
            package.write(path, path.relative_to(app.parent).as_posix())
    with zipfile.ZipFile(output) as package:
        if package.testzip() is not None:
            raise ValueError("ZIP数据校验失败")
        items = package.infolist()
        if not all(item.filename.startswith(APP_NAME + "/") for item in items):
            raise ValueError("ZIP未保留固定应用名")
        if not all(item.filename.isascii() or item.flag_bits & 0x800 for item in items):
            raise ValueError("ZIP中的非ASCII文件名缺少UTF-8标记")
        executable = package.getinfo(EXECUTABLE)
        if not stat.S_IMODE(executable.external_attr >> 16) & 0o111:
            raise ValueError("ZIP未保留主程序可执行权限")
    print("标准ZIP、固定应用名、UTF-8文件名、可执行权限和CRC检查通过。")


if __name__ == "__main__":
    try:
        if len(sys.argv) != 3:
            raise ValueError("需要应用目录和不存在的ZIP输出路径")
        archive(pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]))
    except (OSError, ValueError, zipfile.BadZipFile) as error:
        raise SystemExit(str(error))
