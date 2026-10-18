#!/usr/bin/env python3
"""让电视直接去拉一个远端 URL（HLS/MP4）——验证「服务端出流、板子只做 DLNA 控制点」。

    python3 -I tools/push_url.py --tv 192.168.3.18 --mime application/vnd.apple.mpegurl URL

不起本地 HTTP 服务，数据通路完全不经过本机；只发 Stop / SetAVTransportURI / Play，再轮询 GetTransportInfo。
"""
import argparse, os, re, sys, time, urllib.request
from xml.sax.saxutils import escape

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlna_push as d  # noqa: E402


def transport_state(tv):
    env = ('<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" '
           's:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>'
           f'<u:GetTransportInfo xmlns:u="{d.AVT}"><InstanceID>0</InstanceID></u:GetTransportInfo></s:Body></s:Envelope>')
    req = urllib.request.Request(f"http://{tv}:9197/upnp/control/AVTransport1", env.encode(), {
        "Content-Type": 'text/xml; charset="utf-8"', "SOAPACTION": f'"{d.AVT}#GetTransportInfo"'})
    with urllib.request.urlopen(req, timeout=10) as r:
        return re.search(r"<CurrentTransportState>(.*?)<", r.read().decode()).group(1)


ap = argparse.ArgumentParser()
ap.add_argument("--tv", required=True)
ap.add_argument("--mime", default="video/mp4")
ap.add_argument("--watch", type=int, default=20, help="轮询状态的秒数")
ap.add_argument("url")
a = ap.parse_args()

d.soap(a.tv, "Stop", [("InstanceID", 0)])
d.soap(a.tv, "SetAVTransportURI", [("InstanceID", 0), ("CurrentURI", escape(a.url)),
                                   ("CurrentURIMetaData", d.didl(a.url, "test", a.mime))])
d.soap(a.tv, "Play", [("InstanceID", 0), ("Speed", 1)])
for _ in range(a.watch):
    time.sleep(1)
    print(time.strftime("%H:%M:%S"), transport_state(a.tv), flush=True)
