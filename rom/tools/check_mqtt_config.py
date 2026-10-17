#!/usr/bin/env python3
"""禁止在 rom/main 里调用 esp_mqtt_set_config()。

esp-mqtt 的这个 API 会用传入的结构体**整体覆盖**现有配置。v0.9.4 曾在
CONNECTED/DISCONNECTED 里传一个只填了 reconnect_timeout_ms 的结构体，
结果 broker URI、credentials、TLS 根证书全被清空——第一次连上之后，
每次重连都报 “No server verification option set”，设备永远连不上。

重连退避请用 esp_timer + esp_mqtt_client_reconnect()（见 cloud_client.c
的 reconnect_cb/schedule_reconnect），不要再碰 set_config。

运行：
    python3 tools/check_mqtt_config.py     # 在 rom/ 目录下
"""
import pathlib
import sys


def main() -> int:
    bad = []
    for path in sorted(pathlib.Path("main").glob("*.c")):
        for lineno, line in enumerate(path.read_text().splitlines(), 1):
            stripped = line.lstrip()
            if stripped.startswith(("//", "*", "/*")):
                continue
            if "esp_mqtt_set_config(" in line:
                bad.append(f"{path}:{lineno}: {stripped}")

    if bad:
        print("发现 esp_mqtt_set_config() 调用；它会整体覆盖 MQTT 配置：",
              file=sys.stderr)
        for item in bad:
            print("  " + item, file=sys.stderr)
        print("改用 esp_timer + esp_mqtt_client_reconnect()。", file=sys.stderr)
        return 1
    print("check_mqtt_config: OK（没有 esp_mqtt_set_config 调用）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
