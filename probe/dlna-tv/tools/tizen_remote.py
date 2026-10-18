#!/usr/bin/env python3
"""三星 Tizen 网络遥控（8002/TLS）——验证「软拔插后自动导航回图片」用。

    tools/.venv/bin/python tools/tizen_remote.py --tv 192.168.3.16 pair
    tools/.venv/bin/python tools/tizen_remote.py --tv 192.168.3.16 key KEY_SOURCE KEY_DOWN KEY_ENTER
    tools/.venv/bin/python tools/tizen_remote.py --tv 192.168.3.16 replug-and KEY_ENTER ...

首次连接电视会弹「允许/拒绝」，3 秒内没收到 ms.channel.connect 就是在等你按允许。
token 存在 tools/.tizen_token（已 gitignore）；协议三星没文档化，token 可能会变。
"""
import argparse, base64, json, os, re, ssl, sys, time, urllib.request

import websocket

HERE = os.path.dirname(os.path.abspath(__file__))
TOKEN_FILE = os.path.join(HERE, ".tizen_token")
NAME = base64.b64encode(b"StickBox Probe").decode()


def load_token():
    try:
        return open(TOKEN_FILE).read().strip() or None
    except FileNotFoundError:
        return None


def connect(tv, timeout=30):
    url = f"wss://{tv}:8002/api/v2/channels/samsung.remote.control?name={NAME}"
    tok = load_token()
    if tok:
        url += f"&token={tok}"
    ws = websocket.create_connection(url, timeout=timeout, sslopt={"cert_reqs": ssl.CERT_NONE},
                                    skip_utf8_validation=True)
    t0 = time.time()
    while True:
        msg = json.loads(ws.recv())
        ev = msg.get("event")
        if ev == "ms.channel.connect":
            new = (msg.get("data") or {}).get("token")
            if new and new != tok:
                with open(TOKEN_FILE, "w") as f:
                    f.write(new)
                print(f"已授权，token 已存（{time.time() - t0:.1f}s）")
            return ws
        if ev == "ms.channel.timeOut":
            sys.exit("30 秒内没人在电视上点「允许」，重跑一次 pair")
        if ev == "ms.channel.unauthorized":
            sys.exit("电视拒绝了（按了拒绝，或需要在 设置→常规→外部设备管理→设备连接管理器 里清掉旧记录）")
        print("  事件:", ev)


def read10_calls(probe):
    txt = urllib.request.urlopen(f"http://{probe}/status", timeout=3).read().decode()
    m = re.search(r"read10 调用\s+(\d+)", txt)
    return int(m.group(1)) if m else -1


def wait_quiet(probe, quiet, margin, limit=30.0):
    """等电视读完这一轮：读计数先动起来，再连续 quiet 秒不动，然后多等 margin 秒。
    缩略图要解码整张 JPEG，列表没画完之前按键会被吞——这一步就是为了等它画完。"""
    t0 = time.time()
    base = last = read10_calls(probe)
    changed_at = None
    while time.time() - t0 < limit:
        time.sleep(0.3)
        n = read10_calls(probe)
        if n != last:
            last, changed_at = n, time.time()
        elif changed_at and time.time() - changed_at >= quiet:
            print(f"  电视读完（{last - base} 次 read10），静默 {quiet}s，再等 {margin}s")
            time.sleep(margin)
            return
    print(f"  {limit}s 内没等到静默，直接发键")


def send_keys(ws, keys, gap):
    for k in keys:
        if k.startswith("sleep:"):
            time.sleep(float(k[6:]))
            continue
        ws.send(json.dumps({"method": "ms.remote.control", "params": {
            "Cmd": "Click", "DataOfCmd": k, "Option": "false", "TypeOfRemote": "SendRemoteKey"}}))
        print(f"  → {k}")
        time.sleep(gap)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tv", required=True)
    ap.add_argument("--probe", default="192.168.3.15", help="探针 IP（replug-and 用）")
    ap.add_argument("--gap", type=float, default=0.6, help="按键间隔秒")
    ap.add_argument("--settle", type=float, default=0, help="软拔插后固定等待秒数；0 = 按读计数自动判断")
    ap.add_argument("--quiet", type=float, default=1.5, help="自动模式：读计数静默多久算读完")
    ap.add_argument("--margin", type=float, default=1.0, help="自动模式：读完后再等多久")
    sub = ap.add_subparsers(dest="cmd", required=True)
    sub.add_parser("pair")
    k = sub.add_parser("key"); k.add_argument("keys", nargs="+")
    r = sub.add_parser("replug-and"); r.add_argument("keys", nargs="*")
    a = ap.parse_args()

    if a.cmd == "pair":
        print("连接中——看电视，弹窗就按「允许」…")
        connect(a.tv, timeout=60).close()
        return
    ws = connect(a.tv)
    if a.cmd == "replug-and":
        t0 = time.time()
        urllib.request.urlopen(f"http://{a.probe}/replug?ms=1500", timeout=5).read()
        if a.settle:
            print(f"已触发软拔插，固定等 {a.settle}s…")
            time.sleep(a.settle)
        else:
            print("已触发软拔插，等电视读完…")
            time.sleep(1.6)  # 先让断开那 1.5s 过去，别把旧计数当成新一轮
            wait_quiet(a.probe, a.quiet, a.margin)
        ws.close(); ws = connect(a.tv)  # 等待期间连接可能被电视踢掉，重连一次
        print(f"  t={time.time() - t0:.1f}s 开始发键")
    send_keys(ws, a.keys, a.gap)
    ws.close()


if __name__ == "__main__":
    main()
