#!/usr/bin/env python3
"""常驻直播：电视只拉一条 /live.ts，服务端在流里随时切换素材（不重新编码）。

    python3 -I tools/live_switch.py --chunks /tmp/swap-media/chunks --port 8770
    curl 'http://<本机>:8770/play?clip=B'      # 运行中切到素材 B
    curl 'http://<本机>:8770/status'

素材预先切成 2 秒一段的 TS（按关键帧、流拷贝，规格一致）。每个连接起一个 ffmpeg，
用 concat 读一份很长的清单，清单里每一项都是 slotdir 下的一个符号链接 s{k}.ts——
ffmpeg 读到第 k 段才会打开它，所以我们可以在「第 k 段开始前一刻」才决定它指向哪个素材的哪一段。
ffmpeg 负责把各段时间戳接成连续的一条流（流拷贝），电视看到的就是一条不间断的 TS。
切换延迟 ≈ 1~2 段（2~4 秒）。
"""
import argparse, glob, http.server, os, subprocess, tempfile, threading, time, urllib.parse

SEG = 2.0       # 每段秒数
AHEAD = 2       # 提前多少段决定内容（越小切换越快，越大越不容易让 ffmpeg 开不到文件）
N_SLOTS = 3000  # 清单长度，足够跑 100 分钟


class State:
    def __init__(self):
        self.lock = threading.Lock()
        self.clip = None
        self.epoch = 0   # 每次 /play 加一，连接据此知道要从新素材第 0 段开始

    def set(self, clip):
        with self.lock:
            self.clip = clip
            self.epoch += 1

    def get(self):
        with self.lock:
            return self.clip, self.epoch


def log(msg):
    print(f"{time.strftime('%H:%M:%S')}  {msg}", flush=True)


class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, fmt, *args):
        log(fmt % args)

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
        u = urllib.parse.urlparse(self.path)
        if u.path == "/play":
            clip = urllib.parse.parse_qs(u.query).get("clip", [""])[0]
            if clip not in CLIPS:
                self.send_response(404); self.end_headers(); self.wfile.write(b"unknown clip"); return
            ST.set(clip)
            log(f"== /play clip={clip}")
            self.send_response(200); self.end_headers(); self.wfile.write(b"ok\n"); return
        if u.path == "/status":
            self.send_response(200); self.end_headers()
            self.wfile.write(f"clip={ST.get()[0]} clips={sorted(CLIPS)}\n".encode()); return
        if u.path != "/live.ts":
            self.send_response(404); self.end_headers(); return
        self._hdr()
        self.stream()

    def stream(self):
        slotdir = tempfile.mkdtemp(prefix="slots-")
        lst = os.path.join(slotdir, "list.txt")
        with open(lst, "w") as f:
            for k in range(N_SLOTS):
                f.write(f"file '{slotdir}/s{k}.ts'\n")
        done = threading.Event()
        t0 = time.time()
        pos = {"clip": None, "epoch": -1, "i": 0}

        def assign(k):
            clip, epoch = ST.get()
            if epoch != pos["epoch"]:
                pos["epoch"], pos["clip"], pos["i"] = epoch, clip, 0
            chunks = CLIPS[pos["clip"]]
            src = chunks[pos["i"] % len(chunks)]
            os.symlink(src, f"{slotdir}/s{k}.ts")
            log(f"   段 {k:3d} ← {os.path.basename(src)}")
            pos["i"] += 1

        # 前 AHEAD+1 段必须在 ffmpeg 启动前就绪，之后由后台线程按时间推进
        clip0, _ = ST.get()
        for k in range(AHEAD + 1):
            assign(k)
        threading.Thread(target=self._feed_from, args=(AHEAD + 1, assign, done, t0), daemon=True).start()

        ff = subprocess.Popen(["ffmpeg", "-v", "error", "-re", "-f", "concat", "-safe", "0", "-i", lst,
                               "-c", "copy", "-flush_packets", "1", "-muxdelay", "0", "-f", "mpegts", "-"],
                              stdout=subprocess.PIPE)
        sent = 0
        log(f"连接建立（当前素材 {clip0}）")
        try:
            while True:
                b = ff.stdout.read1(65536)
                if not b:
                    break
                self.wfile.write(b); sent += len(b)
        except (BrokenPipeError, ConnectionResetError):
            pass
        finally:
            done.set(); ff.kill()
            log(f"连接断开（{time.time() - t0:.0f}s，{sent // 1024} KB）")

    @staticmethod
    def _feed_from(nxt, assign, done, t0):
        while not done.is_set():
            now_k = int((time.time() - t0) / SEG)
            while nxt <= now_k + AHEAD:
                assign(nxt); nxt += 1
            time.sleep(0.1)


ap = argparse.ArgumentParser()
ap.add_argument("--chunks", default="/tmp/swap-media/chunks")
ap.add_argument("--port", type=int, default=8770)
A = ap.parse_args()

CLIPS = {}
for p in sorted(glob.glob(os.path.join(A.chunks, "*_*.ts"))):
    CLIPS.setdefault(os.path.basename(p).split("_")[0], []).append(p)
ST = State()
ST.set(sorted(CLIPS)[0])
log(f"素材 { {k: len(v) for k, v in CLIPS.items()} }，初始 {ST.get()[0]}")
http.server.ThreadingHTTPServer(("0.0.0.0", A.port), H).serve_forever()
