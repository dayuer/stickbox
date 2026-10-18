package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// stdinReader 在测试里替换成固定输入。
var stdinReader io.Reader = os.Stdin

type photoRow struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	UploadedAt int64  `json:"uploaded_at"`
	InPlaylist bool   `json:"in_playlist"`
	DurationS  int    `json:"duration_s"`
}

type photoUploadResp struct {
	SHA256     string `json:"sha256"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	PLV        int64  `json:"plv"`
	InPlaylist bool   `json:"in_playlist"`
}

type photoInitResp struct {
	Token     string `json:"init_token"`
	Force     bool   `json:"force"`
	ExpiresAt int64  `json:"expires_at"`
	PLV       int64  `json:"plv"`
}

type devicePhotoState struct {
	Cap      bool     `json:"cap"`
	Ready    bool     `json:"ready"`
	State    string   `json:"state"`
	PLV      int64    `json:"plv"`
	Have     int      `json:"have"`
	Bad      []string `json:"bad"`
	FreeKB   int64    `json:"free_kb"`
	InitDone string   `json:"init_done"`
	Updated  int64    `json:"updated"`
}

func cmdPhoto(args []string) error {
	if len(args) == 0 {
		return errors.New("用法：stickbox-agent photo add [--no-show] [--secs N] <文件...> | " +
			"photo ls | photo rm <sha前缀> | photo init [--force]")
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	if err := requireToken(c); err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return cmdPhotoAdd(c, args[1:])
	case "ls":
		return cmdPhotoList(c, args[1:])
	case "rm":
		return cmdPhotoRemove(c, args[1:])
	case "init":
		return cmdPhotoInit(c, args[1:])
	default:
		return fmt.Errorf("未知的 photo 子命令 %q（add / ls / rm / init）", args[0])
	}
}

func cmdPhotoAdd(c *agentConfig, args []string) error {
	fs := newFlagSet("photo add")
	noShow := fs.Bool("no-show", false, "只上传，不加入播放清单")
	secs := fs.Int("secs", 8, "在清单里的停留秒数（5–600）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("用法：stickbox-agent photo add [--no-show] [--secs N] <文件...>")
	}
	var failed int
	for _, path := range fs.Args() {
		data, ct, cleanup, warn, err := preparePhoto(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s：%v\n", filepath.Base(path), err)
			failed++
			continue
		}
		if warn != "" {
			fmt.Fprintln(os.Stderr, "提示："+warn)
		}
		res, err := uploadPhotoRaw(c, data, ct, !*noShow, *secs)
		cleanup()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s：上传失败：%v\n", filepath.Base(path), err)
			failed++
			continue
		}
		state := "仅上传"
		if !*noShow && res.InPlaylist {
			state = fmt.Sprintf("已上屏（plv %d）", res.PLV)
		}
		fmt.Printf("%s  sha=%s  %s  %s\n",
			filepath.Base(path), shortSHA(res.SHA256), humanSize(res.Size), state)
	}
	if failed > 0 {
		return fmt.Errorf("%d 个文件上传失败", failed)
	}
	return nil
}

func cmdPhotoList(c *agentConfig, args []string) error {
	fs := newFlagSet("photo ls")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cl, err := newAPIClient(c)
	if err != nil {
		return err
	}
	var list struct {
		Photos []photoRow `json:"photos"`
	}
	if err := cl.call("GET", "/api/agent/photos", true, nil, &list); err != nil {
		return fmt.Errorf("读取照片列表失败：%w", err)
	}
	if len(list.Photos) == 0 {
		fmt.Println("还没有照片。用 stickbox-agent photo add <文件> 添加。")
	} else {
		fmt.Printf("%-10s %-10s %-6s %-8s %s\n", "SHA", "大小", "时长", "清单", "上传时间")
		for _, p := range list.Photos {
			in := "未上屏"
			if p.InPlaylist {
				in = "在清单"
			}
			fmt.Printf("%-10s %-10s %-6s %-8s %s\n",
				shortSHA(p.SHA256), humanSize(p.Size),
				fmt.Sprintf("%ds", p.DurationS), in,
				time.Unix(p.UploadedAt, 0).Local().Format("2006-01-02 15:04"))
		}
	}

	// 设备卡状态来自服务端持久化的最新 status.photo。
	var devices struct {
		Devices []deviceRow `json:"devices"`
	}
	if err := cl.call("GET", "/api/agent/devices", true, nil, &devices); err != nil {
		return nil // 照片列表已经显示出来了，设备状态拿不到不算失败
	}
	for _, d := range devices.Devices {
		state := "离线"
		if d.Online {
			state = "在线"
		}
		fmt.Printf("\n设备 %s（%s）\n", d.Dev, state)
		if d.Photo == nil {
			fmt.Println("  照片卡：未上报（老固件或无 SD 构建）")
			continue
		}
		p := d.Photo
		fmt.Printf("  照片卡：ready=%v state=%s plv=%d 已缓存=%d 剩余=%.1fGB\n",
			p.Ready, p.State, p.PLV, p.Have, float64(p.FreeKB)/1024/1024)
		if len(p.Bad) > 0 {
			fmt.Printf("  跳过：%s\n", strings.Join(p.Bad, ", "))
		}
		if p.InitDone != "" {
			fmt.Printf("  已用初始化 token：%s\n", p.InitDone)
		}
	}
	return nil
}

func cmdPhotoRemove(c *agentConfig, args []string) error {
	fs := newFlagSet("photo rm")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("用法：stickbox-agent photo rm <sha256 或唯一前缀>")
	}
	prefix := strings.ToLower(strings.TrimSpace(fs.Arg(0)))
	if len(prefix) < 8 || len(prefix) > 64 {
		return errors.New("请给至少 8 位 sha256 前缀")
	}
	cl, err := newAPIClient(c)
	if err != nil {
		return err
	}
	var list struct {
		Photos []photoRow `json:"photos"`
	}
	if err := cl.call("GET", "/api/agent/photos", true, nil, &list); err != nil {
		return fmt.Errorf("读取照片列表失败：%w", err)
	}
	var hits []photoRow
	for _, p := range list.Photos {
		if strings.HasPrefix(p.SHA256, prefix) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return fmt.Errorf("没有找到 sha 前缀 %s 的照片", prefix)
	case 1:
	default:
		var names []string
		for i, p := range hits {
			if i == 5 {
				break
			}
			names = append(names, shortSHA(p.SHA256))
		}
		return fmt.Errorf("前缀 %s 匹配到 %d 张照片（%s…），请给更长前缀",
			prefix, len(hits), strings.Join(names, ", "))
	}
	var out struct {
		PLV int64 `json:"plv"`
	}
	if err := cl.call("DELETE", "/api/photo/"+hits[0].SHA256, true, nil, &out); err != nil {
		return fmt.Errorf("删除失败：%w", err)
	}
	fmt.Printf("已删除 %s（plv %d）\n", shortSHA(hits[0].SHA256), out.PLV)
	return nil
}

func cmdPhotoInit(c *agentConfig, args []string) error {
	fs := newFlagSet("photo init")
	force := fs.Bool("force", false, "允许格式化“魔数存在但 CRC 损坏”的卡")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("用法：stickbox-agent photo init [--force]")
	}
	reader := bufio.NewReader(stdinReader)
	if !confirm(reader, "照片初始化会清空 SD 卡内所有数据，设备收到授权后会格式化整张卡。输入 yes 继续：") {
		fmt.Println("已取消。")
		return nil
	}
	if *force {
		if !confirm(reader, "--force 还会格式化“已有本方案超级块但 CRC 损坏”的卡，风险更高。再次输入 yes 确认：") {
			fmt.Println("已取消。")
			return nil
		}
	}
	cl, err := newAPIClient(c)
	if err != nil {
		return err
	}
	var out photoInitResp
	if err := cl.call("POST", "/api/photo/init", true,
		map[string]bool{"force": *force}, &out); err != nil {
		return fmt.Errorf("生成初始化授权失败：%w", err)
	}
	fmt.Printf("已生成初始化授权：token %s…，有效至 %s。设备格式化后自动失效。\n",
		shortSHA(out.Token), time.Unix(out.ExpiresAt, 0).Local().Format("2006-01-02 15:04:05"))
	return nil
}

func confirm(r *bufio.Reader, prompt string) bool {
	fmt.Print(prompt)
	line, err := r.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

// —— 上传前处理 ——

var (
	// preparePhoto 可在测试里替换，避免真的调用 sips。
	preparePhoto = preparePhotoFile
	sipsProfile  = "/System/Library/ColorSync/Profiles/sRGB Profile.icc"
)

// preparePhotoFile 返回上传字节、Content-Type、清理函数和提示。
// 原图只读不复制；只有格式/色彩转换才会生成临时文件，并在上传后删除。
func preparePhotoFile(path string) (data []byte, contentType string, cleanup func(), warn string, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", func() {}, "", err
	}
	if st.IsDir() {
		return nil, "", func() {}, "", errors.New("这是目录")
	}
	if st.Size() > 20<<20 {
		return nil, "", func() {}, "", errors.New("文件超过 20MB")
	}
	head := make([]byte, 64)
	f, err := os.Open(path)
	if err != nil {
		return nil, "", func() {}, "", err
	}
	n, _ := io.ReadFull(f, head)
	f.Close()
	head = head[:n]

	ct := "image/jpeg"
	if bytes.HasPrefix(head, []byte("\x89PNG")) {
		ct = "image/png"
	}
	isHEIC := bytes.Contains(head, []byte("ftypheic")) ||
		bytes.Contains(head, []byte("ftypheix")) ||
		strings.EqualFold(filepath.Ext(path), ".heic") ||
		strings.EqualFold(filepath.Ext(path), ".heif")

	tmpFiles := []string{}
	cleanup = func() {
		for _, p := range tmpFiles {
			os.Remove(p)
		}
	}

	src := path
	if isHEIC {
		if runtime.GOOS != "darwin" {
			cleanup()
			return nil, "", func() {}, "", errors.New("HEIC 需要先转成 JPEG（macOS 可用 sips）")
		}
		out, err := os.CreateTemp("", "stickbox-photo-*.jpg")
		if err != nil {
			cleanup()
			return nil, "", func() {}, "", err
		}
		outPath := out.Name()
		out.Close()
		tmpFiles = append(tmpFiles, outPath)
		if err := runSips("-s", "format", "jpeg", src, "--out", outPath); err != nil {
			cleanup()
			return nil, "", func() {}, "", fmt.Errorf("sips 转 HEIC 失败：%w", err)
		}
		src = outPath
		ct = "image/jpeg"
	}

	if runtime.GOOS == "darwin" && ct == "image/jpeg" {
		converted, warning, err := sipsToSRGB(src)
		if err != nil {
			warn = warning
		} else if converted != src {
			tmpFiles = append(tmpFiles, converted)
			src = converted
		} else if warning != "" {
			warn = warning
		}
	}

	data, err = os.ReadFile(src)
	if err != nil {
		cleanup()
		return nil, "", func() {}, "", err
	}
	if len(data) > 20<<20 {
		cleanup()
		return nil, "", func() {}, "", errors.New("转换后仍超过 20MB")
	}
	return data, ct, cleanup, warn, nil
}

// sipsToSRGB 返回要上传的文件路径：已经是 sRGB 时返回原路径；转换失败时
// 返回原路径并附带提示（继续上传，偏色好过传不上去）。
func sipsToSRGB(path string) (string, string, error) {
	out, err := exec.Command("sips", "-g", "profile", path).CombinedOutput()
	if err != nil {
		return path, "读不到色彩配置，按原样上传（广色域照片可能略偏淡）", nil
	}
	if strings.Contains(strings.ToLower(string(out)), "srgb") {
		return path, "", nil
	}
	tmp, err := os.CreateTemp("", "stickbox-srgb-*.jpg")
	if err != nil {
		return path, "无法创建临时文件，按原样上传（广色域照片可能略偏淡）", nil
	}
	tmpPath := tmp.Name()
	tmp.Close()
	if err := runSips("-m", sipsProfile, "-s", "format", "jpeg", path, "--out", tmpPath); err != nil {
		os.Remove(tmpPath)
		return path, "转 sRGB 失败，按原样上传（广色域照片可能略偏淡）", nil
	}
	return tmpPath, "", nil
}

func runSips(args ...string) error {
	out, err := exec.Command("sips", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%w：%s", err, firstLine(msg))
		}
		return err
	}
	return nil
}

func uploadPhotoRaw(c *agentConfig, data []byte, ct string, show bool, secs int) (photoUploadResp, error) {
	var out photoUploadResp
	cl, err := newAPIClient(c)
	if err != nil {
		return out, err
	}
	q := fmt.Sprintf("?show=%d&secs=%d", boolInt(show), secs)
	req, err := http.NewRequest("POST", cl.base+"/api/photo"+q, bytes.NewReader(data))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("User-Agent", "stickbox-agent/"+version)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := cl.hc.Do(req)
	if err != nil {
		return out, fmt.Errorf("连接 %s 失败：%w", cl.base, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var e struct {
			E      string `json:"e"`
			Detail string `json:"detail"`
		}
		json.Unmarshal(raw, &e)
		return out, &apiError{Status: resp.StatusCode, Code: e.E, Detail: e.Detail}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("服务端返回了无法识别的 JSON：%w", err)
	}
	return out, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
