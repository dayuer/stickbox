package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
)

// 崩溃回传（T10）。
//
// 设备的 coredump 分区只有 64K，这里按 256K 封顶——留足余量，又不至于让
// 一个拿到设备密钥的人用大包把磁盘撑爆。
const maxCoredumpBytes = 256 << 10

// 每台设备最多保留几份。超了删最旧的：故障现场里最新的几份最有用，
// 而且磁盘是有限的。
const keepCoredumpsPerDev = 10

// 所有设备的崩溃包加起来的上限。单设备限流挡不住很多台一起慢慢灌。
const maxCoredumpTotalBytes = 512 << 20

// dirBytes 统计目录下所有普通文件的字节数。崩溃包数量不多（每设备最多
// 保留 10 份），每次上传走一遍可以接受。
func dirBytes(root string) int64 {
	var n int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// POST /api/device/coredump —— 设备鉴权，请求体是原始 ELF。
//
// 固件版本和复位原因走头部：服务端不解 ELF，没有版本号就还原不了符号，
// 一份没有版本号的 coredump 等于没有。
func (a *API) handleDeviceCoredump(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	// 只收已配对设备的崩溃包。requireDevice 放行待配对（pending）密钥，
	// 而 /api/device/register 不鉴权——否则一个 IP 每 10 分钟能造 20 个 dev，
	// 每个塞 2.5MB，一天约 7GB，磁盘直接写满。
	// 两道都要：密钥不能是待配对的，设备也必须已经绑定给某个用户。
	// 光看「设备已绑定」不够——知道 MAC 的人可以重新 register 拿一把
	// pending 密钥冒用这台已配对设备；光看「密钥非 pending」也不够。
	if k, ok, err := a.store.KeyByID(id.KeyID); err != nil || !ok || k.Pending {
		fail(w, http.StatusForbidden, "not paired", "")
		return
	}
	if owner, bound, err := a.store.OwnerOfDevice(id.Dev); err != nil || !bound || owner == "" {
		fail(w, http.StatusForbidden, "not paired", "")
		return
	}
	// 每台设备每小时一次。正常设备开机才传一次，传完就擦；高频只可能是
	// 崩溃循环或者有人在灌。
	if !a.coredumpDev.allow(id.Dev) {
		fail(w, http.StatusTooManyRequests, "rate limited", "")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxCoredumpBytes+1))
	if err != nil {
		fail(w, http.StatusBadRequest, "read failed", "")
		return
	}
	if len(body) == 0 {
		fail(w, http.StatusBadRequest, "empty", "")
		return
	}
	if len(body) > maxCoredumpBytes {
		fail(w, http.StatusRequestEntityTooLarge, "too large", "")
		return
	}

	// 全局配额：所有设备的崩溃包加起来不能超过上限。单设备限流挡不住
	// 「很多台设备一起慢慢灌」，这道是兜底。
	root := filepath.Join(a.cfg.Root, "coredumps")
	if dirBytes(root)+int64(len(body)) > maxCoredumpTotalBytes {
		fail(w, http.StatusInsufficientStorage, "quota exceeded", "")
		return
	}

	// 目录在校验全过之后才建，不然被拒的请求也会在磁盘上留下一堆空目录。
	dir := filepath.Join(root, id.Dev)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, "server error", "")
		return
	}

	// 带纳秒后缀：同一秒内传两次会互相覆盖，而崩溃循环恰好就是同一秒连来。
	ts := fmt.Sprintf("%s-%09d", time.Now().UTC().Format("20060102-150405"),
		time.Now().Nanosecond())
	base := filepath.Join(dir, ts)

	// 先写临时文件再改名：设备上传到一半断了的话，不该在目录里留下一份
	// 看起来完整、实际被截断的 ELF。
	tmp := base + ".elf.part"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		fail(w, http.StatusInternalServerError, "write failed", "")
		return
	}
	if err := os.Rename(tmp, base+".elf"); err != nil {
		os.Remove(tmp)
		fail(w, http.StatusInternalServerError, "write failed", "")
		return
	}

	meta := fmt.Sprintf("{\"dev\":%q,\"fw\":%q,\"reset\":%q,\"size\":%d,\"at\":%q}\n",
		id.Dev,
		sanitizeHeader(r.Header.Get("X-Stickbox-Version")),
		sanitizeHeader(r.Header.Get("X-Stickbox-Reset")),
		len(body),
		time.Now().UTC().Format(time.RFC3339))
	os.WriteFile(base+".json", []byte(meta), 0o644)

	pruneCoredumps(dir, keepCoredumpsPerDev)
	writeJSON(w, http.StatusOK, map[string]any{"ok": 1, "size": len(body)})
}

// sanitizeHeader 把设备传来的头部值压成可以安全写进 JSON 和文件名的样子。
// 设备密钥可能泄露，别人就能往这里投任意头部——不能原样落盘。
func sanitizeHeader(s string) string {
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '-' || r == '_' || r == '+' {
			return r
		}
		return -1
	}, s)
}

// pruneCoredumps 只保留最新的 keep 份（按文件名排序，文件名是时间戳）。
func pruneCoredumps(dir string, keep int) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var elfs []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".elf") {
			elfs = append(elfs, e.Name())
		}
	}
	if len(elfs) <= keep {
		return
	}
	sort.Strings(elfs)
	for _, name := range elfs[:len(elfs)-keep] {
		os.Remove(filepath.Join(dir, name))
		os.Remove(filepath.Join(dir, strings.TrimSuffix(name, ".elf")+".json"))
	}
}

// GET /api/agent/coredumps —— agent 鉴权，列出自己名下设备的崩溃记录。
func (a *API) handleAgentCoredumps(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	devs, err := a.store.DevicesOfUser(id.UserID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "server error", "")
		return
	}

	out := []map[string]any{}
	for _, dev := range devs {
		dir := filepath.Join(a.cfg.Root, "coredumps", dev)
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".elf") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, map[string]any{
				"dev":  dev,
				"at":   strings.TrimSuffix(e.Name(), ".elf"),
				"size": info.Size(),
			})
		}
	}
	// 新的在前。
	sort.Slice(out, func(i, j int) bool {
		return out[i]["at"].(string) > out[j]["at"].(string)
	})
	writeJSON(w, http.StatusOK, out)
}
