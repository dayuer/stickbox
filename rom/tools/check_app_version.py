#!/usr/bin/env python3
"""断言 app.bin 里 esp_app_desc.version 等于发布 tag。

ESP-IDF 的 app 描述结构（esp_app_desc_t）以 0xABCD5432 开头，
version 在 magic 之后 16 字节、32 字节定长。描述位于第一个 segment，
这里在前 4KB 内找 magic，避免依赖具体的 segment 布局。

  python3 rom/tools/check_app_version.py build-release/app.bin v0.1.0
"""

from __future__ import annotations

import struct
import sys

MAGIC = 0xABCD5432
VERSION_OFFSET = 16
VERSION_LEN = 32
SCAN_LIMIT = 4096


def app_version(path: str) -> str:
    data = open(path, "rb").read()
    for off in range(0, min(len(data), SCAN_LIMIT) - 4):
        if struct.unpack_from("<I", data, off)[0] == MAGIC:
            raw = data[off + VERSION_OFFSET : off + VERSION_OFFSET + VERSION_LEN]
            return raw.split(b"\0", 1)[0].decode("utf-8", "replace")
    raise SystemExit(f"{path}: 前 {SCAN_LIMIT} 字节里找不到 esp_app_desc magic 0x{MAGIC:08X}")


def main() -> int:
    if len(sys.argv) != 3:
        raise SystemExit(f"用法: {sys.argv[0]} <app.bin> <tag>")
    path, want = sys.argv[1], sys.argv[2]
    got = app_version(path)
    if got != want:
        print(f"app_desc.version={got!r}，发布 tag={want!r}，不一致", file=sys.stderr)
        return 1
    print(f"app_desc.version={got} ✓")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
