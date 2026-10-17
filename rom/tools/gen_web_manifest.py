#!/usr/bin/env python3
"""从 ESP-IDF 的 flasher_args.json 生成 ESP Web Tools 的 manifest.json。

  python3 rom/tools/gen_web_manifest.py \
      --flasher-args build/flasher_args.json \
      --out-dir web/flash \
      --version 1.2.3 \
      --copy-bins

为什么从 flasher_args.json 生成：分区表还会随 T9（OTA 双槽 + frames 分区）
继续改，手写偏移迟早会坏。这里只信 `idf.py build` 的产物。

`--copy-bins` 会把 flash_files 里的 bin 按 basename 复制到输出目录，
manifest 里的 path 也用 basename（GitHub Pages 上与页面同目录）。
不复制时只生成 manifest，供本地检查。

单测：python3 -m unittest discover -s web/tests -v
"""

from __future__ import annotations

import argparse
import json
import os
import shutil
import sys
import tempfile
from pathlib import Path


class ManifestError(Exception):
    """输入不合法。CLI 捕获后以非零退出。"""


# ESP Web Tools 的 chipFamily 写法。
CHIP_FAMILIES = {
    "esp32": "ESP32",
    "esp32s2": "ESP32-S2",
    "esp32s3": "ESP32-S3",
    "esp32c3": "ESP32-C3",
    "esp32c6": "ESP32-C6",
    "esp32h2": "ESP32-H2",
    "esp32p4": "ESP32-P4",
}
DEFAULT_CHIP = "ESP32-S3"


def load_flasher_args(path: str | os.PathLike) -> dict:
    try:
        raw = json.loads(Path(path).read_text(encoding="utf-8"))
    except OSError as e:
        raise ManifestError(f"读不到 flasher_args.json：{e}") from e
    except json.JSONDecodeError as e:
        raise ManifestError(f"flasher_args.json 不是合法 JSON：{e}") from e
    if not isinstance(raw, dict):
        raise ManifestError("flasher_args.json 顶层必须是对象")
    if not isinstance(raw.get("flash_files"), dict):
        raise ManifestError("flasher_args.json 缺少 flash_files")
    return raw


def normalize_chip(raw: str | None) -> str:
    if not raw:
        return DEFAULT_CHIP
    text = str(raw).strip()
    key = text.lower().replace("-", "").replace("_", "")
    return CHIP_FAMILIES.get(key, text)


def _parse_offset(raw) -> int:
    if isinstance(raw, bool):  # bool 是 int 的子类，明确拒绝
        raise ManifestError(f"偏移不是数字：{raw!r}")
    if isinstance(raw, int):
        value = raw
    elif isinstance(raw, str):
        try:
            value = int(raw.strip(), 0)
        except ValueError as e:
            raise ManifestError(f"偏移不是数字：{raw!r}") from e
    else:
        raise ManifestError(f"偏移不是数字：{raw!r}")
    if value < 0:
        raise ManifestError(f"偏移不能为负：{raw!r}")
    return value


def _parts(args: dict) -> list[tuple[int, Path]]:
    """返回按偏移排序的 (offset, 源文件路径) 列表。"""
    flash_files = args.get("flash_files")
    if not isinstance(flash_files, dict) or not flash_files:
        raise ManifestError("flash_files 为空")

    parts: list[tuple[int, Path]] = []
    seen_offsets: set[int] = set()
    basenames: dict[str, Path] = {}
    for offset_raw, bin_path in flash_files.items():
        offset = _parse_offset(offset_raw)
        if offset in seen_offsets:
            raise ManifestError(f"偏移 {offset_raw!r} 重复")
        seen_offsets.add(offset)

        if not isinstance(bin_path, str) or not bin_path.strip():
            raise ManifestError(f"偏移 {offset_raw!r} 的文件名不合法：{bin_path!r}")
        source = Path(bin_path)
        name = source.name
        prev = basenames.get(name)
        if prev is not None and prev.resolve() != source.resolve():
            raise ManifestError(
                f"两个不同的固件文件同名 {name!r}，复制到烧录页会互相覆盖"
            )
        basenames[name] = source
        parts.append((offset, source))

    parts.sort(key=lambda item: item[0])
    if parts[0][0] != 0:
        raise ManifestError("flash_files 里没有偏移 0x0 的 bootloader，无法从空片烧录")
    return parts


def build_manifest(
    args: dict,
    *,
    name: str = "StickBox",
    version: str = "dev",
    chip: str | None = None,
) -> dict:
    parts = _parts(args)
    chip_raw = chip or (args.get("extra_esptool_args") or {}).get("chip")
    return {
        "name": name,
        "version": version,
        "new_install_prompt_erase": True,
        "builds": [
            {
                "chipFamily": normalize_chip(chip_raw),
                "parts": [
                    {"path": source.name, "offset": offset}
                    for offset, source in parts
                ],
            }
        ],
    }


def generate(
    flasher_args_path: str | os.PathLike,
    out_dir: str | os.PathLike,
    *,
    name: str = "StickBox",
    version: str = "dev",
    chip: str | None = None,
    copy_bins: bool = False,
) -> Path:
    args_path = Path(flasher_args_path)
    args = load_flasher_args(args_path)
    manifest = build_manifest(args, name=name, version=version, chip=chip)

    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)

    if copy_bins:
        base = args_path.parent
        for _offset, source in _parts(args):
            src = source if source.is_absolute() else base / source
            if not src.is_file():
                raise ManifestError(f"找不到固件文件：{src}")
            shutil.copyfile(src, out / src.name)

    # 先写临时文件再替换，避免构建中断留下半个 manifest。
    fd, tmp_name = tempfile.mkstemp(prefix=".manifest-", suffix=".json", dir=out)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(manifest, f, ensure_ascii=False, indent=2)
            f.write("\n")
        manifest_path = out / "manifest.json"
        os.replace(tmp_name, manifest_path)
    except BaseException:
        try:
            os.unlink(tmp_name)
        except OSError:
            pass
        raise
    return manifest_path


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="从 flasher_args.json 生成 ESP Web Tools manifest.json"
    )
    parser.add_argument("--flasher-args", required=True, help="build/flasher_args.json")
    parser.add_argument("--out-dir", required=True, help="输出目录（通常是 web/flash）")
    parser.add_argument("--name", default="StickBox", help="显示名称")
    parser.add_argument("--version", default="dev", help="固件版本（Release 标签）")
    parser.add_argument(
        "--chip", default=None, help="芯片型号；默认从 extra_esptool_args.chip 推断"
    )
    parser.add_argument(
        "--copy-bins", action="store_true", help="把 bin 复制到输出目录"
    )
    ns = parser.parse_args(argv)

    try:
        path = generate(
            ns.flasher_args,
            ns.out_dir,
            name=ns.name,
            version=ns.version,
            chip=ns.chip,
            copy_bins=ns.copy_bins,
        )
    except ManifestError as e:
        print(f"错误：{e}", file=sys.stderr)
        return 1
    print(f"已生成 {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
