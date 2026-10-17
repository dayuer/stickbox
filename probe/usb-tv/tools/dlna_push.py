#!/usr/bin/env python3
"""用 DLNA/UPnP AVTransport 把图片直接推给电视（不走 USB）——对比软拔插方案的切换体验。

    python3 -I tools/dlna_push.py --tv 192.168.3.16 --interval 6 assets/slide1.jpg assets/slide2.jpg ...

本机起一个临时 HTTP 服务供电视拉图；每张图依次 SetAVTransportURI + Play。
"""
import argparse, functools, http.server, os, socket, threading, time, urllib.error, urllib.request
from xml.sax.saxutils import escape

AVT = "urn:schemas-upnp-org:service:AVTransport:1"


def soap(tv, action, args):
    body = "".join(f"<{k}>{v}</{k}>" for k, v in args)
    env = ('<?xml version="1.0" encoding="utf-8"?>'
           '<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" '
           's:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>'
           f'<u:{action} xmlns:u="{AVT}">{body}</u:{action}></s:Body></s:Envelope>')
    req = urllib.request.Request(f"http://{tv}:9197/upnp/control/AVTransport1", env.encode(), {
        "Content-Type": 'text/xml; charset="utf-8"', "SOAPACTION": f'"{AVT}#{action}"'})
    try:
        with urllib.request.urlopen(req, timeout=10) as r:
            return r.status
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")
        # 图片在 SetAVTransportURI 时就已经显示了，没有可 Play 的状态迁移，电视回 701——不算失败
        if action in ("Play", "Stop", "Next") and "<errorCode>701<" in detail:
            return 701
        raise SystemExit(f"{action} 失败 {e.code}: {detail}")


def didl(url, title, mime="image/jpeg"):
    cls = "object.item.videoItem" if mime.startswith("video/") else "object.item.imageItem.photo"
    item = ('<DIDL-Lite xmlns="urn:schemas-upnp-org:metadata-1-0/DIDL-Lite/" '
            'xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:upnp="urn:schemas-upnp-org:metadata-1-0/upnp/">'
            f'<item id="0" parentID="-1" restricted="1"><dc:title>{escape(title)}</dc:title>'
            f'<upnp:class>{cls}</upnp:class>'
            f'<res protocolInfo="http-get:*:{mime}:*">{escape(url)}</res></item></DIDL-Lite>')
    return escape(item)


def my_ip(tv):
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.connect((tv, 9))
    return s.getsockname()[0]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tv", required=True)
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--interval", type=float, default=6)
    ap.add_argument("--mode", choices=["stop", "next"], default="stop",
                    help="stop: Stop+SetAVTransportURI（每次都会弹底部工具条）；"
                         "next: 首张照常，之后 SetNextAVTransportURI+Next，看能否不弹工具条")
    ap.add_argument("--mime", default="image/jpeg", help="例如 video/avi（推 MJPEG 视频流）")
    ap.add_argument("files", nargs="+")
    a = ap.parse_args()

    root = os.path.dirname(os.path.abspath(a.files[0]))
    handler = functools.partial(http.server.SimpleHTTPRequestHandler, directory=root)
    srv = http.server.ThreadingHTTPServer(("0.0.0.0", a.port), handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    base = f"http://{my_ip(a.tv)}:{a.port}"

    for i, f in enumerate(a.files):
        name = os.path.basename(f)
        url = f"{base}/{name}?n={i}"  # 每次 URL 不同，避免电视按 URL 缓存
        t0 = time.time()
        if a.mode == "next" and i > 0:
            soap(a.tv, "SetNextAVTransportURI", [("InstanceID", 0), ("NextURI", escape(url)),
                                                 ("NextURIMetaData", didl(url, name, a.mime))])
            rc = soap(a.tv, "Next", [("InstanceID", 0)])
            print(f"→ {name}  Next{' (701)' if rc == 701 else ''}  ({time.time() - t0:.2f}s)", flush=True)
        else:
            # 正在显示图片时直接换 URI 会回 701；先 Stop 回到 STOPPED 才允许 SetAVTransportURI
            soap(a.tv, "Stop", [("InstanceID", 0)])
            soap(a.tv, "SetAVTransportURI", [("InstanceID", 0), ("CurrentURI", escape(url)),
                                             ("CurrentURIMetaData", didl(url, name, a.mime))])
            soap(a.tv, "Play", [("InstanceID", 0), ("Speed", 1)])
            print(f"→ {name}  ({time.time() - t0:.2f}s)", flush=True)
        time.sleep(a.interval)
    srv.shutdown()


if __name__ == "__main__":
    main()
