# T17 实验工具：可调 AVI 头 / HTTP 头 / fps / burst，日志含 GET 请求头
import sys, os, time, threading, http.server, urllib.request, re, argparse, struct
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import live_avi as L, dlna_push as D
from xml.sax.saxutils import escape

BIG = 0x7FFFFFF0

ap = argparse.ArgumentParser()
ap.add_argument("--tv", required=True)
ap.add_argument("--burst", type=int, default=8)
ap.add_argument("--port", type=int, default=8110)
ap.add_argument("--fps", type=float, default=2)
ap.add_argument("--duration", type=float, default=40)
ap.add_argument("--frame", required=True)
ap.add_argument("--variant", default="base")
ap.add_argument("--content-length", type=int, default=0, help="0=不发")
ap.add_argument("--http11", action="store_true")
ap.add_argument("--op", default="00")
ap.add_argument("--flags", default="01700000000000000000000000000000")
ap.add_argument("--transfer", default="Streaming")
ap.add_argument("--mime", default="video/avi")
a = ap.parse_args()

f = open(a.frame, "rb").read()
w, h = L.jpeg_size(f)
fps = a.fps

# ---- AVI 头变体 ----
def header():
    total = BIG
    maxbps = int(len(f) * fps)
    sug = len(f) + 8
    length = BIG
    riff = BIG
    movi = BIG
    bisize = w * h * 3
    if a.variant == "totalframes0":
        total = 0
    elif a.variant == "totalframes100k":
        total = 100000
    elif a.variant == "maxbps0":
        maxbps = 0
    elif a.variant == "maxbpsbig":
        maxbps = BIG
    elif a.variant == "suggested0":
        sug = 0
    elif a.variant == "suggested3x":
        sug = w * h * 3
    elif a.variant == "length0":
        length = 0
    elif a.variant == "riff0":
        riff = movi = 0
    elif a.variant == "bisizejpg":
        bisize = len(f)

    avih = struct.pack("<IIIIIIIIII4I", int(1e6 / fps), maxbps, 0, 0, total, 0, 1,
                       sug, w, h, 0, 0, 0, 0)
    strh = struct.pack("<4s4sIHHIIIIIIIIhhhh", b"vids", b"MJPG", 0, 0, 0, 0, 1,
                       int(round(fps)), 0, length, sug, 0xFFFFFFFF, 0, 0, 0, w, h)
    strf = struct.pack("<IiiHH4sIiiII", 40, w, h, 1, 24, b"MJPG", bisize, 0, 0, 0, 0)
    strl = b"LIST" + struct.pack("<I", 4 + len(L.chunk(b"strh", strh)) + len(L.chunk(b"strf", strf))) + \
        b"strl" + L.chunk(b"strh", strh) + L.chunk(b"strf", strf)
    hdrl_body = b"hdrl" + L.chunk(b"avih", avih) + strl
    hdrl = b"LIST" + struct.pack("<I", len(hdrl_body)) + hdrl_body
    return b"RIFF" + struct.pack("<I", riff) + b"AVI " + hdrl + b"LIST" + struct.pack("<I", movi) + b"movi"

HEAD = header()
T0 = [0.0]
events = []

def log(s):
    events.append((time.time() - T0[0], s))
    print(f"{time.time()-T0[0]:6.2f}s {s}", flush=True)

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1" if a.http11 else "HTTP/1.0"
    def log_message(self, *x): pass
    def hdr(self):
        self.send_response(200)
        self.send_header("Content-Type", a.mime)
        self.send_header("transferMode.dlna.org", a.transfer)
        self.send_header("contentFeatures.dlna.org",
                         f"DLNA.ORG_OP={a.op};DLNA.ORG_CI=0;DLNA.ORG_FLAGS={a.flags}")
        if a.content_length:
            self.send_header("Content-Length", str(a.content_length))
        self.end_headers()
    def do_HEAD(self):
        log("HEAD " + str(dict(self.headers)))
        self.hdr()
    def do_GET(self):
        log("GET 开始 " + str(dict(self.headers)))
        self.hdr()
        n = 0; sent = 0; t = time.time()
        try:
            self.wfile.write(HEAD)
            while time.time() - t < a.duration:
                self.wfile.write(L.chunk(b"00dc", f))
                n += 1; sent += len(f)
                if n in (1, 2, 4, 8, 16, 20) or n % 40 == 0:
                    log(f"已发 {n} 帧 {sent//1024}KB")
                if n > a.burst:
                    time.sleep(max(0, t + (n - a.burst) / fps - time.time()))
        except Exception as e:
            log(f"连接断开 {type(e).__name__}")

srv = http.server.ThreadingHTTPServer(("0.0.0.0", a.port), H)
threading.Thread(target=srv.serve_forever, daemon=True).start()
url = f"http://{D.my_ip(a.tv)}:{a.port}/live.avi"

def state():
    env = ('<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>'
           f'<u:GetTransportInfo xmlns:u="{D.AVT}"><InstanceID>0</InstanceID></u:GetTransportInfo></s:Body></s:Envelope>')
    r = urllib.request.Request(f"http://{a.tv}:9197/upnp/control/AVTransport1", env.encode(),
                               {"Content-Type": 'text/xml; charset="utf-8"', "SOAPACTION": f'"{D.AVT}#GetTransportInfo"'})
    return re.search(r"<CurrentTransportState>(\w+)<", urllib.request.urlopen(r, timeout=5).read().decode()).group(1)

try:
    D.soap(a.tv, "Stop", [("InstanceID", 0)])
except SystemExit:
    pass
time.sleep(2)
T0[0] = time.time()
log(f"开始推送 variant={a.variant} burst={a.burst} fps={fps} cl={a.content_length} http11={a.http11} op={a.op} flags={a.flags[:6]} transfer={a.transfer}")
D.soap(a.tv, "SetAVTransportURI", [("InstanceID", 0), ("CurrentURI", escape(url)),
                                   ("CurrentURIMetaData", D.didl(url, "StickBox", a.mime))])
log("SetURI 返回")
D.soap(a.tv, "Play", [("InstanceID", 0), ("Speed", 1)])
log("Play 返回")
last = None
while time.time() - T0[0] < a.duration + 10:
    try:
        s = state()
    except Exception:
        s = "ERR"
    if s != last:
        log(f"状态 {s}")
        last = s
    time.sleep(0.25)
try:
    D.soap(a.tv, "Stop", [("InstanceID", 0)])
except SystemExit:
    pass
