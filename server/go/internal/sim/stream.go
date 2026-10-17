package sim

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"net"
	"net/http"
	"sync"
	"time"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

const (
	screenW = 1920
	screenH = 1080
	// AVI 里的“很大”值：直播流没有确定的帧数/文件长度。
	bigSize = 0x7FFFFFF0
	// 预留的单帧上限，AVI 头里的 maxframe。
	maxAVIFrame = 512 << 10
)

// waitingJPEG 生成「等待配对 / 配对码」画面。尺寸与看板帧一致（1920×1080），
// 这样电视端的 AVI 头在切到真帧时不用换。
func waitingJPEG(code string, paired bool) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, screenW, screenH))
	bg := color.RGBA{0x1d, 0x2d, 0x3d, 0xff}
	grid := color.RGBA{0x2c, 0x45, 0x5d, 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	for x := 72; x < screenW; x += 120 {
		draw.Draw(img, image.Rect(x, 0, x+1, screenH), &image.Uniform{grid}, image.Point{}, draw.Src)
	}
	for y := 40; y < screenH; y += 120 {
		draw.Draw(img, image.Rect(0, y, screenW, y+1), &image.Uniform{grid}, image.Point{}, draw.Src)
	}

	ink := color.RGBA{0xf5, 0xf5, 0xf8, 0xff}
	muted := color.RGBA{0x94, 0xbc, 0xe3, 0xff}
	drawCentered(img, "STICKBOX", 8, 150, ink)
	drawCentered(img, "PAIR THIS DEVICE", 5, 330, muted)

	if code == "" {
		code = "------"
	}
	// 大字号的 6 位配对码。
	drawCentered(img, spaced(code), 20, 470, ink)

	status := "WAITING FOR FRAME"
	if paired {
		status = "PAIRED · WAITING FOR FRAME"
	}
	drawCentered(img, status, 5, 850, muted)
	drawCentered(img, "stickbox-agent pair "+code, 4, 940, muted)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func spaced(s string) string {
	out := make([]byte, 0, len(s)*2)
	for i := 0; i < len(s); i++ {
		if i > 0 {
			out = append(out, ' ')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// drawCentered 用 basicfont 渲染 ASCII，再按 scale 最近邻放大。
func drawCentered(dst *image.RGBA, text string, scale, y int, col color.Color) {
	face := basicfont.Face7x13
	w := font.MeasureString(face, text).Ceil()
	h := face.Metrics().Height.Ceil()
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	d := font.Drawer{
		Dst:  src,
		Src:  &image.Uniform{col},
		Face: face,
		Dot:  fixed.P(0, face.Metrics().Ascent.Ceil()),
	}
	d.DrawString(text)

	big := image.NewRGBA(image.Rect(0, 0, w*scale, h*scale))
	xdraw.NearestNeighbor.Scale(big, big.Bounds(), src, src.Bounds(), draw.Over, nil)
	x := (screenW - big.Bounds().Dx()) / 2
	draw.Draw(dst, image.Rect(x, y, x+big.Bounds().Dx(), y+big.Bounds().Dy()),
		big, image.Point{}, draw.Over)
}

// jpegSize 读 JPEG 的 SOF 段拿宽高。
func jpegSize(b []byte) (int, int, error) {
	i := 2
	for i+9 < len(b) {
		if b[i] != 0xFF {
			i++
			continue
		}
		m := b[i+1]
		if m == 0xC0 || m == 0xC1 || m == 0xC2 {
			h := int(binary.BigEndian.Uint16(b[i+5 : i+7]))
			w := int(binary.BigEndian.Uint16(b[i+7 : i+9]))
			return w, h, nil
		}
		if i+4 > len(b) {
			break
		}
		i += 2 + int(binary.BigEndian.Uint16(b[i+2:i+4]))
	}
	return 0, 0, fmt.Errorf("sim: 找不到 JPEG SOF")
}

func aviChunk(fcc string, data []byte) []byte {
	pad := 0
	if len(data)%2 == 1 {
		pad = 1
	}
	out := make([]byte, 8+len(data)+pad)
	copy(out[:4], fcc)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(data)))
	copy(out[8:], data)
	return out
}

// aviHeader 按 T17 的 live_avi.py 生成 AVI 头（帧数/长度写成 BIG）。
func aviHeader(w, h, fps, maxFrame int) []byte {
	usPerFrame := 1000000 / fps
	avih := make([]byte, 0, 56)
	put32 := func(v uint32) {
		avih = append(avih, make([]byte, 4)...)
		binary.LittleEndian.PutUint32(avih[len(avih)-4:], v)
	}
	put32(uint32(usPerFrame))
	put32(uint32(maxFrame * fps))
	put32(0)
	put32(0)
	put32(bigSize)
	put32(0)
	put32(1)
	put32(uint32(maxFrame))
	put32(uint32(w))
	put32(uint32(h))
	put32(0)
	put32(0)
	put32(0)
	put32(0)

	strh := make([]byte, 0, 56)
	strh = append(strh, []byte("vidsMJPG")...)
	add32 := func(v uint32) {
		strh = append(strh, make([]byte, 4)...)
		binary.LittleEndian.PutUint32(strh[len(strh)-4:], v)
	}
	add16 := func(v uint16) {
		strh = append(strh, make([]byte, 2)...)
		binary.LittleEndian.PutUint16(strh[len(strh)-2:], v)
	}
	add32(0) // flags
	add16(0) // priority
	add16(0) // language
	add32(0) // initial frames
	add32(1) // scale
	add32(uint32(fps))
	add32(0)       // start
	add32(bigSize) // length
	add32(uint32(maxFrame))
	add32(0xFFFFFFFF) // quality
	add32(0)          // sample size
	add16(0)          // rcFrame left
	add16(0)          // top
	add16(uint16(w))  // right
	add16(uint16(h))  // bottom

	strf := make([]byte, 40)
	binary.LittleEndian.PutUint32(strf[0:4], 40)
	binary.LittleEndian.PutUint32(strf[4:8], uint32(w))
	binary.LittleEndian.PutUint32(strf[8:12], uint32(h))
	binary.LittleEndian.PutUint16(strf[12:14], 1)
	binary.LittleEndian.PutUint16(strf[14:16], 24)
	copy(strf[16:20], "MJPG")
	binary.LittleEndian.PutUint32(strf[20:24], uint32(w*h*3))
	// 其余 xpels/ypels/clrUsed/clrImportant 都是 0

	strlBody := append([]byte("strl"), aviChunk("strh", strh)...)
	strlBody = append(strlBody, aviChunk("strf", strf)...)
	strl := append([]byte("LIST"), le32(uint32(len(strlBody)))...)
	strl = append(strl, strlBody...)

	hdrlBody := append([]byte("hdrl"), aviChunk("avih", avih)...)
	hdrlBody = append(hdrlBody, strl...)
	hdrl := append([]byte("LIST"), le32(uint32(len(hdrlBody)))...)
	hdrl = append(hdrl, hdrlBody...)

	out := append([]byte("RIFF"), le32(bigSize)...)
	out = append(out, []byte("AVI ")...)
	out = append(out, hdrl...)
	out = append(out, []byte("LIST")...)
	out = append(out, le32(bigSize)...)
	out = append(out, []byte("movi")...)
	return out
}

func le32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// streamServer 是电视拉的 /live.avi。HTTP/1.0、不发 Content-Length，
// 靠连接保持直播（T17 的结论）。
type streamServer struct {
	addr    string
	fps     int
	burst   int
	current func() []byte
	ln      net.Listener
	srv     *http.Server
	actual  string

	mu    sync.Mutex
	conns map[net.Conn]*streamConn
}

type streamConn struct {
	started   time.Time
	lastWrite time.Time
}

// 最近一次成功写帧超过这个时间，就不能再算「正在拉流」。
// 帧率 2fps，正常电视每 500ms 收一帧。
const streamActiveWindow = 3 * time.Second

func newStreamServer(addr string, fps, burst int, current func() []byte) *streamServer {
	return &streamServer{addr: addr, fps: fps, burst: burst, current: current}
}

func (s *streamServer) Start() error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.ln = ln
	s.actual = ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("/live.avi", s.handle)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	return nil
}

func (s *streamServer) Addr() string {
	if s == nil {
		return ""
	}
	return s.actual
}

func (s *streamServer) Close() error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

func (s *streamServer) track(c net.Conn) {
	now := time.Now()
	s.mu.Lock()
	if s.conns == nil {
		s.conns = map[net.Conn]*streamConn{}
	}
	s.conns[c] = &streamConn{started: now, lastWrite: now}
	s.mu.Unlock()
}

func (s *streamServer) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *streamServer) markWrite(c net.Conn) {
	s.mu.Lock()
	if st := s.conns[c]; st != nil {
		st.lastWrite = time.Now()
	}
	s.mu.Unlock()
}

// ActiveFor 返回最久的拉流连接已经持续了多久；没有连接返回 0。
func (s *streamServer) ActiveFor() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var best time.Duration
	for _, st := range s.conns {
		if d := now.Sub(st.started); d > best {
			best = d
		}
	}
	return best
}

// Active 表示有连接在最近 streamActiveWindow 内成功收到过帧。
func (s *streamServer) Active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, st := range s.conns {
		if now.Sub(st.lastWrite) <= streamActiveWindow {
			return true
		}
	}
	return false
}

func (s *streamServer) headers() string {
	return "HTTP/1.0 200 OK\r\n" +
		"Content-Type: video/avi\r\n" +
		"transferMode.dlna.org: Streaming\r\n" +
		"contentFeatures.dlna.org: DLNA.ORG_OP=00;DLNA.ORG_CI=0;" +
		"DLNA.ORG_FLAGS=01700000000000000000000000000000\r\n" +
		"Connection: close\r\n\r\n"
}

func (s *streamServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/live.avi" {
		http.NotFound(w, r)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	s.track(conn)
	defer func() {
		s.untrack(conn)
		conn.Close()
	}()

	if _, err := buf.WriteString(s.headers()); err != nil {
		return
	}
	if err := buf.Flush(); err != nil {
		return
	}
	if r.Method == http.MethodHead {
		return
	}

	frame := s.current()
	w0, h0, err := jpegSize(frame)
	if err != nil {
		w0, h0 = screenW, screenH
	}
	head := aviHeader(w0, h0, s.fps, maxAVIFrame)
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(head); err != nil {
		return
	}

	t0 := time.Now()
	n := 0
	for {
		frame = s.current()
		conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := conn.Write(aviChunk("00dc", frame)); err != nil {
			return
		}
		s.markWrite(conn)
		n++
		if n > s.burst {
			due := t0.Add(time.Duration(n-s.burst) * time.Second / time.Duration(s.fps))
			if d := time.Until(due); d > 0 {
				time.Sleep(d)
			}
		}
	}
}

// localIPToward 返回「连电视时本机用的那个局域网 IP」。
// 不能用监听地址（":8091" / "[::]:8091" 对电视不可达）。
func localIPToward(host string) (string, error) {
	conn, err := net.Dial("udp", net.JoinHostPort(host, "1900"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	if !usableLANIP(ip) {
		return "", fmt.Errorf("sim: 连电视时拿到的本机地址不可用：%v", ip)
	}
	return ip.String(), nil
}

func usableLANIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsLoopback()
}

// buildStreamURL 用局域网 IP + 监听端口拼直播地址；主机名永远取
// 「连电视时的本机 IP」，不从 listenAddr 里取。
func buildStreamURL(listenAddr, tv string, ipFor func(string) (string, error)) (string, error) {
	ipStr, err := ipFor(tv)
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(ipStr)
	if !usableLANIP(ip) {
		return "", fmt.Errorf("sim: 无效的局域网地址 %q", ipStr)
	}
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("sim: 监听地址 %q 不合法：%w", listenAddr, err)
	}
	if port == "" {
		return "", fmt.Errorf("sim: 监听地址缺少端口：%q", listenAddr)
	}
	return "http://" + net.JoinHostPort(ip.String(), port) + "/live.avi", nil
}
