#!/usr/bin/env python3
"""生成 SOAP golden：逐字节复刻 T17 probe/dlna-tv/tools/dlna_push.py 的
SetAVTransportURI 请求体，供 Go 侧测试比对。

    python3 -I gen_soap_golden.py --dlna-push ../../../../../../probe/dlna-tv/tools/dlna_push.py \
        --out soap_setavtransporturi.xml
"""
import argparse
import importlib.util

AVT = "urn:schemas-upnp-org:service:AVTransport:1"
URL = "http://192.168.3.237:8091/live.avi"


def load(path):
    spec = importlib.util.spec_from_file_location("dlna_push", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dlna-push", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()

    m = load(a.dlna_push)
    args = [
        ("InstanceID", 0),
        ("CurrentURI", m.escape(URL)),
        ("CurrentURIMetaData", m.didl(URL, "StickBox", "video/avi")),
    ]
    body = "".join(f"<{k}>{v}</{k}>" for k, v in args)
    env = ('<?xml version="1.0" encoding="utf-8"?>'
           '<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" '
           's:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>'
           f'<u:SetAVTransportURI xmlns:u="{AVT}">{body}</u:SetAVTransportURI>'
           '</s:Body></s:Envelope>')
    with open(a.out, "wb") as f:
        f.write(env.encode())


if __name__ == "__main__":
    main()
