"""烧录页资源检查：脚本必须本地、字体不依赖第三方。"""

import re
import unittest
from html.parser import HTMLParser
from pathlib import Path

FLASH = Path(__file__).resolve().parents[1] / "flash"
VENDOR_VERSION = "10.4.0"


class _Collector(HTMLParser):
    def __init__(self):
        super().__init__()
        self.scripts = []
        self.links = []

    def handle_starttag(self, tag, attrs):
        d = dict(attrs)
        if tag == "script" and d.get("src"):
            self.scripts.append(d["src"])
        if tag == "link" and d.get("href"):
            rel = tuple((d.get("rel") or "").split())
            self.links.append((rel, d["href"]))


def parse_page():
    c = _Collector()
    c.feed((FLASH / "index.html").read_text(encoding="utf-8"))
    return c


class PageAssetsTest(unittest.TestCase):
    def test_no_external_scripts(self):
        for src in parse_page().scripts:
            self.assertFalse(
                src.startswith(("http://", "https://", "//")),
                f"不允许从外部加载脚本：{src}",
            )
            self.assertTrue((FLASH / src).is_file(), f"本地脚本不存在：{src}")

    def test_no_external_styles_or_fonts(self):
        for _rel, href in parse_page().links:
            self.assertFalse(
                href.startswith(("http://", "https://", "//")),
                f"不允许外部资源：{href}",
            )

    def test_page_has_no_cdn_or_font_references(self):
        html = (FLASH / "index.html").read_text(encoding="utf-8")
        self.assertNotIn("unpkg.com", html)
        self.assertNotIn("fonts.googleapis", html)
        self.assertNotIn("fonts.gstatic", html)

    def test_vendor_bundle_is_complete(self):
        vendor = FLASH / "vendor" / f"esp-web-tools-{VENDOR_VERSION}"
        entry = vendor / "install-button.js"
        self.assertTrue(entry.is_file(), "缺少本地 install-button.js")
        self.assertTrue((vendor / "LICENSE").is_file(), "缺少 esp-web-tools 的 LICENSE")
        js = entry.read_text(encoding="utf-8")
        for m in re.finditer(r'import\("(\./[^"]+)"\)', js):
            self.assertTrue(
                (vendor / m.group(1)).is_file(),
                f"缺少 install-button.js 动态 import 的 chunk：{m.group(1)}",
            )


if __name__ == "__main__":
    unittest.main()
