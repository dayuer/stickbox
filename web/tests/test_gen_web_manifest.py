"""gen_web_manifest.py 的单元测试。

跑法：
    python3 -m unittest discover -s web/tests -v
"""

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "rom" / "tools"))

import gen_web_manifest as g  # noqa: E402

FIXTURE = Path(__file__).parent / "fixtures" / "flasher_args.json"


def fixture_args():
    return json.loads(FIXTURE.read_text(encoding="utf-8"))


def write_fixture_tree(tmp: Path) -> Path:
    """在临时目录里造一份 flasher_args.json 和它引用的 bin。"""
    bins = {
        "bootloader/bootloader.bin": b"BOOT",
        "partition_table/partition-table.bin": b"PART",
        "ota_data_initial.bin": b"OTA0",
        "stickbox.bin": b"APP!",
    }
    for rel, data in bins.items():
        p = tmp / "build" / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(data)
    fa = tmp / "build" / "flasher_args.json"
    fa.write_text(json.dumps(fixture_args()), encoding="utf-8")
    return fa


class BuildManifestTest(unittest.TestCase):
    def test_sample_fixture_maps_offsets_and_basenames(self):
        manifest = g.build_manifest(fixture_args(), name="StickBox", version="1.2.3")
        self.assertEqual(manifest["name"], "StickBox")
        self.assertEqual(manifest["version"], "1.2.3")
        self.assertTrue(manifest["new_install_prompt_erase"])

        build = manifest["builds"][0]
        self.assertEqual(build["chipFamily"], "ESP32-S3")
        self.assertEqual(
            [p["offset"] for p in build["parts"]],
            [0x0, 0x8000, 0xF000, 0x20000],
        )
        self.assertEqual(
            [p["path"] for p in build["parts"]],
            [
                "bootloader.bin",
                "partition-table.bin",
                "ota_data_initial.bin",
                "stickbox.bin",
            ],
        )

    def test_chip_override(self):
        manifest = g.build_manifest(
            fixture_args(), name="StickBox", version="dev", chip="ESP32-S3"
        )
        self.assertEqual(manifest["builds"][0]["chipFamily"], "ESP32-S3")

        manifest = g.build_manifest(
            fixture_args(), name="StickBox", version="dev", chip="esp32c6"
        )
        self.assertEqual(manifest["builds"][0]["chipFamily"], "ESP32-C6")

    def test_bad_offset_rejected(self):
        args = fixture_args()
        args["flash_files"]["nope"] = "stickbox.bin"
        with self.assertRaises(g.ManifestError):
            g.build_manifest(args, name="StickBox", version="dev")

    def test_missing_bootloader_rejected(self):
        args = fixture_args()
        del args["flash_files"]["0x0"]
        with self.assertRaises(g.ManifestError):
            g.build_manifest(args, name="StickBox", version="dev")

    def test_duplicate_basename_rejected(self):
        args = fixture_args()
        args["flash_files"]["0x10000"] = "other/stickbox.bin"
        args["flash_files"]["0x20000"] = "stickbox.bin"
        with self.assertRaises(g.ManifestError):
            g.build_manifest(args, name="StickBox", version="dev")

    def test_empty_flash_files_rejected(self):
        with self.assertRaises(g.ManifestError):
            g.build_manifest({"flash_files": {}}, name="StickBox", version="dev")


class WriteManifestTest(unittest.TestCase):
    def test_writes_manifest_and_copies_bins(self):
        with tempfile.TemporaryDirectory() as td:
            tmp = Path(td)
            args_path = write_fixture_tree(tmp)
            out = tmp / "web" / "flash"

            manifest_path = g.generate(
                args_path, out, name="StickBox", version="9.9.9", copy_bins=True
            )

            self.assertEqual(manifest_path, out / "manifest.json")
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            self.assertEqual(manifest["version"], "9.9.9")
            self.assertEqual(len(manifest["builds"][0]["parts"]), 4)
            self.assertEqual((out / "stickbox.bin").read_bytes(), b"APP!")
            self.assertEqual(
                (out / "bootloader.bin").read_bytes(),
                b"BOOT",
            )
            self.assertFalse((out / "bootloader").exists())

    def test_copy_missing_bin_fails(self):
        with tempfile.TemporaryDirectory() as td:
            tmp = Path(td)
            args_path = write_fixture_tree(tmp)
            (tmp / "build" / "stickbox.bin").unlink()
            with self.assertRaises(g.ManifestError):
                g.generate(
                    args_path,
                    tmp / "out",
                    name="StickBox",
                    version="dev",
                    copy_bins=True,
                )

    def test_manifest_only_does_not_need_bins(self):
        with tempfile.TemporaryDirectory() as td:
            tmp = Path(td)
            out = tmp / "out"
            manifest_path = g.generate(
                FIXTURE, out, name="StickBox", version="dev", copy_bins=False
            )
            self.assertTrue(manifest_path.exists())
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            self.assertEqual(len(manifest["builds"][0]["parts"]), 4)


if __name__ == "__main__":
    unittest.main()
