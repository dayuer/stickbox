#!/usr/bin/env python3
"""从 app.bin 生成服务端 OTA manifest 片段。

服务端 manifest 的格式是 {version, url, sha256, size}，其中 url 由部署方
填写；这里产出其余三项，版本必须与 app_desc.version 一致。

  python3 rom/tools/make_ota_manifest.py build-release/app.bin v0.1.0 out.json
"""

from __future__ import annotations

import hashlib
import json
import sys


def main() -> int:
    if len(sys.argv) != 4:
        raise SystemExit(f"用法: {sys.argv[0]} <app.bin> <tag> <out.json>")
    app, version, out = sys.argv[1], sys.argv[2], sys.argv[3]
    data = open(app, "rb").read()
    manifest = {
        "version": version,
        "sha256": hashlib.sha256(data).hexdigest(),
        "size": len(data),
    }
    with open(out, "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print(f"{out}: {manifest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
