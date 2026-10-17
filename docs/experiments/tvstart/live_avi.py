#!/usr/bin/env python3
"""永不结束的 MJPEG-AVI 直播流 + DLNA 推送——模拟 ESP32 将来要做的事。

    python3 -I tools/live_avi.py --tv 192.168.3.16 --fps 2 --cycle 8 f1.jpg f2.jpg f3.jpg

电视只推一次 /live.avi；之后服务端按 fps 重复发「当前帧」，换内容 = 下一帧换成另一张 JPEG。
这正是 ESP32 的工作量：每张图只编码一次，之后只是重复发同一块字节，不需要视频编码器。

控制：GET /set?i=N 立即切到第 N 张；--cycle 秒数 > 0 时自动轮换（演示用）。
JPEG 必须是同一分辨率、baseline、yuvj420p（ffmpeg -pix_fmt yuvj420p 生成）。
"""
import argparse, http.server, os, struct, sys, threading, time, urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlna_push  # noqa: E402

BIG = 0x7FFFFFF0  # 帧数 / 块长度写成「很大」，播放器就当它是一段很长的视频


def jpeg_size(b):
    i = 2
    while i < len(b):
        if b[i] != 0xFF:
            i += 1; continue
        m = b[i + 1]
        if m in (0xC0, 0xC1, 0xC2):
            h, w = struct.unpack(">HH", b[i + 5:i + 9])
            return w, h
        i += 2 + struct.unpack(">H", b[i + 2:i + 4])[0]
    raise ValueError("no SOF")


def chunk(fcc, data):
    pad = b"\0" if len(data) & 1 else b""
    return fcc + struct.pack("<I", len(data)) + data + pad


def avi_header(w, h, fps, maxframe):
    avih = struct.pack("<IIIIIIIIII4I", int(1e6 / fps), maxframe * fps, 0, 0, BIG, 0, 1,
                       maxframe, w, h, 0, 0, 0, 0)
    strh = struct.pack("<4s4sIHHIIIIIIIIhhhh", b"vids", b"MJPG", 0, 0, 0, 0, 1, fps, 0, BIG,
                       maxframe, 0xFFFFFFFF, 0, 0, 0, w, h)
    strf = struct.pack("<IiiHH4sIiiII", 40, w, h, 1, 24, b"MJPG", w * h * 3, 0, 0, 0, 0)
    strl = b"LIST" + struct.pack("<I", 4 + len(chunk(b"strh", strh)) + len(chunk(b"strf", strf))) + \
        b"strl" + chunk(b"strh", strh) + chunk(b"strf", strf)
    hdrl_body = b"hdrl" + chunk(b"avih", avih) + strl
    hdrl = b"LIST" + struct.pack("<I", len(hdrl_body)) + hdrl_body
    return b"RIFF" + struct.pack("<I", BIG) + b"AVI " + hdrl + b"LIST" + struct.pack("<I", BIG) + b"movi"


class State:
    def __init__(self, frames):
        self.frames, self.cur, self.lock = frames, 0, threading.Lock()

    def set(self, i):
        with self.lock:
            self.cur = i % len(self.frames)
        print(f"  [切到第 {self.cur + 1} 张]", flush=True)


def make_handler(st, fps):
    w, h = jpeg_size(st.frames[0])
    head = avi_header(w, h, fps, max(len(f) for f in st.frames) + 8)

    class H(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.0"  # 不带长度，靠断连结束——直播流

        def log_message(self, fmt, *args):
            print(f"  {self.client_address[0]} {fmt % args}", flush=True)

        def _hdr(self):
            self.send_response(200)
            self.send_header("Content-Type", "video/avi")
            self.send_header("transferMode.dlna.org", "Streaming")
            self.send_header("contentFeatures.dlna.org", "DLNA.ORG_OP=00;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
            self.end_headers()

        def do_HEAD(self):
            self._hdr()

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            if u.path == "/set":
                st.set(int(urllib.parse.parse_qs(u.query).get("i", ["0"])[0]))
                self.send_response(204); self.end_headers(); return
            if u.path != "/live.avi":
                self.send_response(404); self.end_headers(); return
            self._hdr()
            t0, n = time.time(), 0
            try:
                self.wfile.write(head)
                while True:
                    with st.lock:
                        f = st.frames[st.cur]
                    self.wfile.write(chunk(b"00dc", f))
                    n += 1
                    time.sleep(max(0, t0 + n / fps - time.time()))
            except (BrokenPipeError, ConnectionResetError):
                print(f"  流断开（发了 {n} 帧，{time.time() - t0:.0f}s）", flush=True)
    return H


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tv", required=True)
    ap.add_argument("--port", type=int, default=8766)
    ap.add_argument("--fps", type=int, default=2)
    ap.add_argument("--cycle", type=float, default=0, help="自动轮换间隔秒；0 = 只靠 /set")
    ap.add_argument("--duration", type=float, default=60, help="演示总时长秒")
    ap.add_argument("frames", nargs="+")
    a = ap.parse_args()

    st = State([open(p, "rb").read() for p in a.frames])
    srv = http.server.ThreadingHTTPServer(("0.0.0.0", a.port), make_handler(st, a.fps))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"http://{dlna_push.my_ip(a.tv)}:{a.port}/live.avi"
    print(f"流地址 {url}  （{a.fps} fps，帧 {len(st.frames[0]) // 1024} KB）", flush=True)

    dlna_push.soap(a.tv, "Stop", [("InstanceID", 0)])
    dlna_push.soap(a.tv, "SetAVTransportURI", [("InstanceID", 0), ("CurrentURI", dlna_push.escape(url)),
                                               ("CurrentURIMetaData", dlna_push.didl(url, "live", "video/avi"))])
    dlna_push.soap(a.tv, "Play", [("InstanceID", 0), ("Speed", 1)])
    print("→ 已推送", flush=True)

    t0 = time.time()
    while time.time() - t0 < a.duration:
        time.sleep(a.cycle or 1)
        if a.cycle:
            st.set(st.cur + 1)
    # 先让电视自己停，再断流——直接断流电视会报「网络中断」（实测，网络其实没断）
    dlna_push.soap(a.tv, "Stop", [("InstanceID", 0)])
    time.sleep(1)
    srv.shutdown()


if __name__ == "__main__":
    main()
