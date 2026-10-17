package sim

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// 推流参数按 T17：HTTP/1.0、video/avi、不发 Content-Length、Streaming、
// OP=00、FLAGS=017…；body 是 RIFF/AVI + 00dc MJPEG 块。
func TestAVIStreamT17Parameters(t *testing.T) {
	jpg, err := waitingJPEG("123456", false)
	if err != nil {
		t.Fatal(err)
	}
	s := NewStreamServer("127.0.0.1:0", 2, 8, func() []byte { return jpg })
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	conn, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /live.avi HTTP/1.0\r\nHost: test\r\n\r\n")

	var data []byte
	tmp := make([]byte, 32<<10)
	for len(data) < 256<<10 {
		n, err := conn.Read(tmp)
		data = append(data, tmp[:n]...)
		if err != nil {
			break
		}
	}

	i := bytes.Index(data, []byte("\r\n\r\n"))
	if i < 0 {
		t.Fatalf("没有读到完整的 HTTP 头：%d 字节", len(data))
	}
	head := string(data[:i])
	body := data[i+4:]

	for _, want := range []string{
		"HTTP/1.0 200 OK",
		"Content-Type: video/avi",
		"transferMode.dlna.org: Streaming",
		"DLNA.ORG_OP=00",
		"DLNA.ORG_FLAGS=01700000000000000000000000000000",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("响应头缺少 %q：\n%s", want, head)
		}
	}
	if strings.Contains(strings.ToLower(head), "content-length") {
		t.Errorf("直播流不该发 Content-Length：\n%s", head)
	}
	if len(body) < 12 || string(body[:4]) != "RIFF" || string(body[8:12]) != "AVI " {
		t.Fatalf("AVI 头不对：%q", body[:min(16, len(body))])
	}
	if !bytes.Contains(body, []byte("movi")) {
		t.Fatal("AVI 缺少 movi")
	}
	j := bytes.Index(body, []byte("00dc"))
	if j < 0 || j+8 > len(body) {
		t.Fatal("AVI 里没有 00dc 帧块")
	}
	chunkLen := int(body[j+4]) | int(body[j+5])<<8 | int(body[j+6])<<16 | int(body[j+7])<<24
	if chunkLen != len(jpg) {
		t.Errorf("00dc 长度 = %d，期望 %d", chunkLen, len(jpg))
	}
	if j+8+len(jpg) > len(body) {
		t.Fatalf("一帧没发完：块头在 %d，body %d", j, len(body))
	}
	if !bytes.Equal(body[j+8:j+8+len(jpg)], jpg) {
		t.Error("00dc 里的 JPEG 与当前帧不一致")
	}

	// 推送成功判定依赖这里：电视连上后 ActiveFor > 0、Active=true；
	// 断开后要从活跃表里移除。
	if s.ActiveFor() <= 0 || !s.Active() {
		t.Errorf("拉流中应算活跃：age=%s active=%v", s.ActiveFor(), s.Active())
	}
	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for s.Active() || s.ActiveFor() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("断开后连接仍在活跃表：age=%s", s.ActiveFor())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
