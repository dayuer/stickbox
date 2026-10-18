#!/usr/bin/env python3
"""服务端实时出一路无限长 H.264+AAC 的 MPEG-TS（带计时器的测试图），电视 HTTP 拉流。

    python3 -I tools/ts_server.py --port 8768      # 流地址 http://<本机>:8768/live.ts
    python3 -I tools/push_url.py --tv 192.168.3.18 --mime video/mpeg http://<本机>:8768/live.ts

每个连接起一个 ffmpeg（-re 实时），stdout 原样转给电视。真实场景把 lavfi 输入换成合成/混流管线即可。
"""
import argparse, http.server, subprocess, time


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, fmt, *args):
        print(f"  {self.client_address[0]} {fmt % args}", flush=True)

    def _hdr(self):
        self.send_response(200)
        self.send_header("Content-Type", "video/mpeg")
        self.send_header("transferMode.dlna.org", "Streaming")
        self.send_header("contentFeatures.dlna.org",
                         "DLNA.ORG_OP=00;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
        self.end_headers()

    def do_HEAD(self):
        self._hdr()

    def do_GET(self):
        if self.path.split("?")[0] != "/live.ts":
            self.send_response(404); self.end_headers(); return
        self._hdr()
        ff = subprocess.Popen(
            ["ffmpeg", "-v", "error", "-re", "-f", "lavfi", "-i", f"testsrc2=size={A.size}:rate=30",
             "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
             "-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency", "-pix_fmt", "yuv420p",
             "-profile:v", "high", "-b:v", "6M", "-maxrate", "6M", "-bufsize", "6M", "-g", "60", "-bf", "0",
             "-x264-params", "repeat-headers=1", "-c:a", "aac", "-b:a", "128k",
             "-flush_packets", "1", "-muxdelay", "0", "-f", "mpegts", "-"],
            stdout=subprocess.PIPE)
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
            ff.kill()
            print(f"  流断开（{time.time() - t0:.0f}s，{sent / 1024:.0f} KB，"
                  f"均 {sent / 1024 / max(1, time.time() - t0):.0f} KB/s）", flush=True)


ap = argparse.ArgumentParser()
ap.add_argument("--port", type=int, default=8768)
ap.add_argument("--size", default="1920x1080")
A = ap.parse_args()
http.server.ThreadingHTTPServer(("0.0.0.0", A.port), H).serve_forever()
