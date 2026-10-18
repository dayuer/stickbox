#!/usr/bin/env python3
"""验证「同一条 TS 直播流里中途换素材，电视会不会断流重来」。

    python3 -I tools/swap_server.py --dir /tmp/swap-media --port 8769

  /naive.ts   A.ts、B.ts 的原始字节直接首尾相接（时间戳回退、CC 断档）
  /remux.ts   ffmpeg concat 流拷贝重封装（时间戳连续，不重新编码）

按每个文件 12 秒的节奏实时发送（naive 手动限速，remux 用 -re）。
每个新 GET 都会打印一行——一次换片之后若出现新的 GET，就是电视断流重来了。
"""
import argparse, http.server, os, subprocess, tempfile, time

CLIP_SECONDS = 12


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, fmt, *args):
        print(f"{time.strftime('%H:%M:%S')}  {fmt % args}", flush=True)

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
        name = self.path.split("?")[0]
        if name not in ("/naive.ts", "/remux.ts"):
            self.send_response(404); self.end_headers(); return
        self._hdr()
        t0, sent = time.time(), 0
        try:
            if name == "/naive.ts":
                sent = self.naive()
            else:
                sent = self.remux()
        except (BrokenPipeError, ConnectionResetError):
            pass
        print(f"{time.strftime('%H:%M:%S')}  {name} 断开（{time.time() - t0:.0f}s，{sent // 1024} KB）", flush=True)

    def naive(self):
        clips = [open(os.path.join(A.dir, n), "rb").read() for n in ("A.ts", "B.ts")]
        sent, i, t0 = 0, 0, time.time()
        while True:
            data = clips[i % 2]
            print(f"{time.strftime('%H:%M:%S')}  naive 切到 {'AB'[i % 2]}", flush=True)
            n = 188 * 100
            for off in range(0, len(data), n):
                self.wfile.write(data[off:off + n]); sent += min(n, len(data) - off)
                # 把这个文件均匀摊到 CLIP_SECONDS 秒里
                target = t0 + i * CLIP_SECONDS + CLIP_SECONDS * (off + n) / len(data)
                time.sleep(max(0, target - time.time()))
            i += 1

    def remux(self):
        lst = os.path.join(tempfile.mkdtemp(), "list.txt")
        with open(lst, "w") as f:
            for i in range(200):
                f.write(f"file '{os.path.join(A.dir, 'AB'[i % 2] + '.ts')}'\n")
        ff = subprocess.Popen(["ffmpeg", "-v", "error", "-re", "-f", "concat", "-safe", "0", "-i", lst,
                               "-c", "copy", "-flush_packets", "1", "-muxdelay", "0", "-f", "mpegts", "-"],
                              stdout=subprocess.PIPE)
        sent = 0
        try:
            while True:
                b = ff.stdout.read1(65536)
                if not b:
                    break
                self.wfile.write(b); sent += len(b)
        finally:
            ff.kill()
        return sent


ap = argparse.ArgumentParser()
ap.add_argument("--dir", default="/tmp/swap-media")
ap.add_argument("--port", type=int, default=8769)
A = ap.parse_args()
http.server.ThreadingHTTPServer(("0.0.0.0", A.port), H).serve_forever()
