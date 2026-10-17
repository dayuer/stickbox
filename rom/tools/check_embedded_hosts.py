#!/usr/bin/env python3
"""校验固件里编进去的云端主机名。

v0.9.5 出过一次事故：发布构建用了本机一份被 gitignore 的 cloud_creds.h，
里面还是旧服务器 silkline，设备上板后 DNS 解析失败、连不上云端。
发布脚本必须自己检查，不能指望“构建成功”等于“地址对”。

规则：
  - 扫描固件里的 ASCII 字符串，找出 mqtts:// 和 https:// 后面的主机名；
  - 忽略占位符/格式串（空主机、含 %、以 . 开头）；
  - 每个主机名都必须等于允许列表中的一项，否则退出码 1。

用法：
    python3 tools/check_embedded_hosts.py build-release/stickbox.bin mqtt.stickbox.ai
"""
import re
import sys

URL_RE = re.compile(rb"(?:mqtts|https)://([A-Za-z0-9.-]+)")


def main() -> int:
    if len(sys.argv) < 3:
        print("用法: check_embedded_hosts.py <firmware.bin> <host> [host...]",
              file=sys.stderr)
        return 2
    path, allowed = sys.argv[1], set(sys.argv[2:])
    data = open(path, "rb").read()

    hosts = set()
    for m in URL_RE.finditer(data):
        host = m.group(1).decode("ascii", "ignore").strip(".")
        if not host or "." not in host:
            continue
        hosts.add(host)

    if not hosts:
        print("✗ 固件里没有找到任何 mqtts:// 或 https:// 主机名", file=sys.stderr)
        return 1

    bad = sorted(h for h in hosts if h not in allowed)
    if bad:
        print("✗ 固件包含非白名单主机:", ", ".join(bad), file=sys.stderr)
        print("  允许:", ", ".join(sorted(allowed)), file=sys.stderr)
        print("  多半是用了本机 cloud_creds.h；发布构建必须用官方地址。",
              file=sys.stderr)
        return 1

    print("check_embedded_hosts: OK（" + ", ".join(sorted(hosts)) + "）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
