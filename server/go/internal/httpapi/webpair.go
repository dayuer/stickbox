package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/metrics"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

// 网页配对（W7）：电视二维码 → 手机 /p?d=<dev> 填 6 位码 → 拿到一条一次性命令
// `stickbox-agent pair --link <url>` → 电脑兑换。
//
// 手机只拿到兑换链接，永远拿不到 agent 令牌。令牌在兑换时才签发
// （用户、令牌、转正设备都在 /api/agent/redeem 里一次完成），所以链接过期
// 不会留下「已绑定却没人持有令牌」的孤儿设备，库里也不存任何可用凭据。

const (
	redeemTTL    = 15 * time.Minute
	redeemPrefix = "/r/"
)

var devRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

// handleWebPair：POST /api/web/pair {dev, code}
func (a *API) handleWebPair(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dev  string `json:"dev"`
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ip := rateKeyIP(r)
	devOK := devRe.MatchString(in.Dev)
	if a.webPairFailIP.exceeded(ip) || a.pairFailWorld.exceeded("global") ||
		(devOK && a.webPairFailDev.exceeded(in.Dev)) {
		fail(w, 429, "rate limited", "")
		return
	}
	failPair := func() {
		metrics.IncPairFail()
		a.webPairFailIP.hit(ip)
		a.pairFailWorld.hit("global")
		if devOK {
			a.webPairFailDev.hit(in.Dev)
		}
	}
	bad := func(code int) {
		failPair()
		fail(w, code, "bad code", "")
	}
	if !devOK || len(in.Code) != 6 {
		bad(400)
		return
	}
	sum := sha256.Sum256([]byte(in.Code))
	hash := hex.EncodeToString(sum[:])
	nowT := time.Now()

	// 码只对它所属的设备有效：先只读查一遍，dev 不符直接当错码，不动别人的码。
	peek, ok, err := a.store.PairCodeByHash(hash)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok || peek.Dev != in.Dev {
		bad(404)
		return
	}
	// 原子消费：并发提交同一个码只有一个拿到行。
	pc, ok, err := a.store.ConsumePairCode(hash, nowT.Unix())
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		bad(404)
		return
	}
	if pc.Dev != in.Dev { // 读与消费之间码被换成了别的设备（不应发生，防御）
		a.store.RestorePairCode(pc)
		bad(404)
		return
	}
	k, ok, err := a.store.KeyByID(pc.KeyID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok || k.Disabled || !k.Pending || k.Role != string(auth.RoleDevice) || k.Dev != pc.Dev {
		bad(409)
		return
	}
	_, bound, err := a.store.OwnerOfDevice(pc.Dev)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if bound {
		// 已绑定的屏只有原主人的 agent 能转正；网页路径总是新用户，拒绝并保住原码。
		a.store.RestorePairCode(pc)
		failPair()
		fail(w, 409, "device bound", "该屏已绑定；请用原主人的 agent 配对")
		return
	}

	raw := make([]byte, 32) // 256 bit
	if _, err := rand.Read(raw); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	secret := base64.RawURLEncoding.EncodeToString(raw)
	if err := a.store.PutPairRedeem(store.PairRedeem{
		LinkHash: linkHash(raw), Dev: pc.Dev, KeyID: pc.KeyID,
		Expires: nowT.Add(redeemTTL).Unix(),
	}); err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]any{
		"link":       a.publicBase(r) + redeemPrefix + secret,
		"expires_in": int(redeemTTL.Seconds()),
	})
}

func linkHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// parseRedeemLink 从完整链接里取出 32 字节密钥；形状不对一律 false。
func parseRedeemLink(link string) ([]byte, bool) {
	if len(link) > 512 {
		return nil, false
	}
	u, err := url.Parse(link)
	if err != nil || !strings.HasPrefix(u.Path, redeemPrefix) {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(u.Path[len(redeemPrefix):])
	if err != nil || len(raw) != 32 {
		return nil, false
	}
	return raw, true
}

// handleAgentRedeem：POST /api/agent/redeem {link}（无令牌）
//
// 链接单次有效：原子删除后才继续，之后任何失败都不会让它复活。
// 所有「链接不可用」的原因（格式、不存在、过期、已用）统一回 404 bad link。
func (a *API) handleAgentRedeem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Link string `json:"link"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	ip := rateKeyIP(r)
	if a.redeemFailIP.exceeded(ip) || a.redeemFailWorld.exceeded("global") {
		fail(w, 429, "rate limited", "")
		return
	}
	failRedeem := func() {
		a.redeemFailIP.hit(ip)
		a.redeemFailWorld.hit("global")
	}
	bad := func(int) {
		failRedeem()
		fail(w, 404, "bad link", "")
	}
	raw, ok := parseRedeemLink(in.Link)
	if !ok {
		bad(404)
		return
	}
	rd, ok, err := a.store.ConsumePairRedeem(linkHash(raw), time.Now().Unix())
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok {
		bad(404)
		return
	}
	name := in.Name
	if name == "" {
		name = "link"
	}
	a.completePair(w, store.PairCode{Dev: rd.Dev, KeyID: rd.KeyID}, nil, name, false, failRedeem, bad)
}

// publicBase 返回对外站点根：配置优先，否则按请求推断
// （只在来源是回环反代时信任 X-Forwarded-Proto）。
func (a *API) publicBase(r *http.Request) string {
	if u := strings.TrimRight(a.cfg.PublicURL, "/"); u != "" {
		return u
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if ip := addrIP(r.RemoteAddr); ip != nil && ip.IsLoopback() &&
		r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// handleWebPairPage：GET /p。零外部资源；脚本与样式同源独立文件，CSP 不放行内联。
func (a *API) handleWebPairPage(w http.ResponseWriter, r *http.Request) {
	webHeaders(w, "text/html; charset=utf-8")
	w.Write([]byte(webPairHTML))
}

func (a *API) handleWebPairJS(w http.ResponseWriter, r *http.Request) {
	webHeaders(w, "text/javascript; charset=utf-8")
	w.Write([]byte(webPairJS))
}

func (a *API) handleWebPairCSS(w http.ResponseWriter, r *http.Request) {
	webHeaders(w, "text/css; charset=utf-8")
	w.Write([]byte(webPairCSS))
}

func webHeaders(w http.ResponseWriter, ct string) {
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
			"base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
}

const webPairHTML = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="referrer" content="no-referrer">
<title>配对 StickBox</title>
<link rel="stylesheet" href="/p/app.css">
</head><body><main>
<h1>配对 StickBox</h1>
<p id="dev" class="muted"></p>
<form id="f" autocomplete="off">
<label for="code">电视上的 6 位配对码</label>
<input id="code" inputmode="numeric" pattern="[0-9]*" maxlength="6" autocomplete="one-time-code" required>
<button id="go" type="submit">提交</button>
</form>
<p id="err" role="alert"></p>
<section id="ok" hidden>
<p>配对码已确认。在已安装 stickbox-agent 的电脑终端里执行下面这条命令，15 分钟内有效，只能用一次：</p>
<pre id="cmd"></pre>
<button id="copy" type="button">复制命令</button>
</section>
</main><script src="/p/app.js"></script></body></html>
`

const webPairCSS = `*{box-sizing:border-box}
body{margin:0;font:16px/1.5 -apple-system,system-ui,"PingFang SC","Noto Sans SC",sans-serif;background:#f6f7f9;color:#111}
main{max-width:28rem;margin:0 auto;padding:1.5rem 1rem}
h1{font-size:1.4rem;margin:0 0 .5rem}
.muted{color:#666;margin:0 0 1rem}
label{display:block;margin-bottom:.4rem}
input{width:100%;font-size:2rem;letter-spacing:.4em;text-align:center;padding:.6rem;border:1px solid #bbb;border-radius:.5rem}
button{display:block;width:100%;margin-top:.8rem;padding:.8rem;font-size:1.1rem;border:0;border-radius:.5rem;background:#1769ff;color:#fff}
button:disabled{opacity:.5}
#err{color:#c00;min-height:1.5em}
pre{white-space:pre-wrap;word-break:break-all;background:#fff;border:1px solid #ddd;border-radius:.5rem;padding:.8rem;font-size:.85rem}
[hidden]{display:none}
`

const webPairJS = `(function(){
var q=new URLSearchParams(location.search),d=q.get("d")||"";
var dev=/^[0-9a-f]{12}$/.test(d)?d:"";
var $=function(i){return document.getElementById(i)};
$("dev").textContent=dev?"设备 …"+dev.slice(-4):"缺少设备信息，请重新扫描电视上的二维码";
if(!dev){$("go").disabled=true;$("code").disabled=true}
var msg={400:"请输入 6 位数字",404:"配对码不对或已过期，请看电视上的最新码",409:"这台屏现在无法这样配对",429:"尝试次数太多，请稍后再试"};
$("f").addEventListener("submit",function(e){
e.preventDefault();
var c=$("code").value.trim();$("err").textContent="";
if(!/^[0-9]{6}$/.test(c)){$("err").textContent=msg[400];return}
$("go").disabled=true;
fetch("/api/web/pair",{method:"POST",headers:{"Content-Type":"application/json"},credentials:"omit",referrerPolicy:"no-referrer",body:JSON.stringify({dev:dev,code:c})})
.then(function(r){return r.json().catch(function(){return{}}).then(function(j){return[r.status,j]})})
.then(function(a){
if(a[0]===200&&a[1].link){
$("cmd").textContent="stickbox-agent pair --link '"+a[1].link+"'";
$("f").hidden=true;$("ok").hidden=false;
}else{$("err").textContent=msg[a[0]]||"出错了，请稍后再试";$("go").disabled=false}
}).catch(function(){$("err").textContent="网络错误，请重试";$("go").disabled=false});
});
$("copy").addEventListener("click",function(){
var t=$("cmd").textContent;
function fallback(){var r=document.createRange();r.selectNodeContents($("cmd"));var s=getSelection();s.removeAllRanges();s.addRange(r);try{document.execCommand("copy");$("copy").textContent="已复制"}catch(x){$("copy").textContent="请长按命令手动复制"}}
if(navigator.clipboard&&navigator.clipboard.writeText){navigator.clipboard.writeText(t).then(function(){$("copy").textContent="已复制"},fallback)}else{fallback()}
});
})();
`
