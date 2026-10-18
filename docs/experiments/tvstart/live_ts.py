#!/usr/bin/env python3
"""永不结束的 4K H.264 / MPEG-TS 直播流 + DLNA 推送。

MJPEG 在这台电视（UA60DU7000）上只到 1080p，4K MJPEG 报「格式不支持」；
要 4K 清晰度只能走硬解的 H.264/HEVC。这里用本机 ffmpeg 实时编码验证电视侧行为，
编码放在哪（云端 / 局域网）另议——ESP32 编不动 4K H.264。

    python3 -I tools/live_ts.py --tv 192.168.3.18 --cycle 8 uhd1.jpg uhd2.jpg uhd3.jpg

每个连接起一个 ffmpeg：Python 按 fps 往它的 stdin 灌「当前 JPEG」，它的 stdout（TS）原样转给电视。
"""
import argparse, http.server, os, subprocess, sys, threading, time, urllib.parse

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import dlna_push  # noqa: E402
from live_avi import State  # noqa: E402


def make_handler(st, a):
    class H(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.0"

        def log_message(self, fmt, *args):
            print(f"  {self.client_address[0]} {fmt % args}", flush=True)

        def _hdr(self):
            self.send_response(200)
            self.send_header("Content-Type", a.mime)
            self.send_header("transferMode.dlna.org", "Streaming")
            self.send_header("contentFeatures.dlna.org",
                             "DLNA.ORG_OP=00;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
            self.end_headers()

        def do_HEAD(self):
            self._hdr()

        def do_GET(self):
            u = urllib.parse.urlparse(self.path)
            if u.path == "/set":
                st.set(int(urllib.parse.parse_qs(u.query).get("i", ["0"])[0]))
                self.send_response(204); self.end_headers(); return
            if u.path != "/live.ts":
                self.send_response(404); self.end_headers(); return
            self._hdr()
            ff = subprocess.Popen(
                ["ffmpeg", "-v", "error", "-f", "image2pipe", "-framerate", str(a.fps), "-c:v", "mjpeg", "-i", "-",
                 "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-pix_fmt", "yuv420p",
                 "-profile:v", "high", "-level", "5.1", "-crf", str(a.crf), "-g", str(a.fps * 2), "-bf", "0",
                 "-x264-params", "repeat-headers=1", "-flush_packets", "1", "-muxdelay", "0", "-f", "mpegts", "-"],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE)
            stop = threading.Event()

            def feed():
                t0, n = time.time(), 0
                try:
                    while not stop.is_set():
                        with st.lock:
                            f = st.frames[st.cur]
                        ff.stdin.write(f); ff.stdin.flush()
                        n += 1
                        time.sleep(max(0, t0 + n / a.fps - time.time()))
                except (BrokenPipeError, ValueError):
                    pass
            threading.Thread(target=feed, daemon=True).start()
            t0, sent = time.time(), 0
            try:
                while True:
                    b = ff.stdout.read1(65536)
                    if not b:
                        break
                    self.wfile.write(b); sent += len(b)
            except (BrokenPipeError, ConnectionResetError):
                pass
            finally:
                stop.set(); ff.kill()
                print(f"  流断开（{time.time() - t0:.0f}s，{sent / 1024:.0f} KB，"
                      f"均 {sent / 1024 / max(1, time.time() - t0):.0f} KB/s）", flush=True)
    return H


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--tv", required=True)
    ap.add_argument("--port", type=int, default=8767)
    ap.add_argument("--fps", type=int, default=5)
    ap.add_argument("--crf", type=int, default=20)
    ap.add_argument("--mime", default="video/mpeg")
    ap.add_argument("--cycle", type=float, default=0)
    ap.add_argument("--duration", type=float, default=60)
    ap.add_argument("frames", nargs="+")
    a = ap.parse_args()

    st = State([open(p, "rb").read() for p in a.frames])
    srv = http.server.ThreadingHTTPServer(("0.0.0.0", a.port), make_handler(st, a))
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    url = f"http://{dlna_push.my_ip(a.tv)}:{a.port}/live.ts"
    print(f"流地址 {url}  （{a.fps} fps，crf {a.crf}）", flush=True)

    dlna_push.soap(a.tv, "Stop", [("InstanceID", 0)])
    dlna_push.soap(a.tv, "SetAVTransportURI", [("InstanceID", 0), ("CurrentURI", dlna_push.escape(url)),
                                               ("CurrentURIMetaData", dlna_push.didl(url, "live", a.mime))])
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
