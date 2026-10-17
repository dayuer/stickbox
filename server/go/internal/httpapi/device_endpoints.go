package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

const pairCodeTTL = 10 * time.Minute

// newPairCode 生成 6 位数字码（允许前导 0）和它的 sha256。
func newPairCode() (code, hash string, err error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", "", err
	}
	code = fmt.Sprintf("%06d", n.Int64())
	sum := sha256.Sum256([]byte(code))
	return code, hex.EncodeToString(sum[:]), nil
}

// issuePairCode 给一台设备写一个新配对码。code_hash 有唯一约束：
// 撞上别的设备的码就换一个重试。
func (a *API) issuePairCode(dev, keyID string, now time.Time) (string, int64, error) {
	a.store.DeleteExpiredPairCodes(now.Unix())
	expires := now.Add(pairCodeTTL).Unix()
	for i := 0; i < 10; i++ {
		code, hash, err := newPairCode()
		if err != nil {
			return "", 0, err
		}
		err = a.store.PutPairCode(store.PairCode{
			Dev: dev, CodeHash: hash, KeyID: keyID,
			Expires: expires, Created: now.Unix(),
		})
		if err == nil {
			return code, expires, nil
		}
	}
	return "", 0, errors.New("pair code collision")
}

// handleDeviceRegister：POST /api/device/register {dev}
//
// 不鉴权。设备被擦掉 NVS 后重新注册时，旧密钥**不吊销**：
// 新密钥处于待配对，只有原主人的 agent 输码才能转正；否则知道 MAC
// 就能把别人的屏顶掉。
func (a *API) handleDeviceRegister(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Dev string `json:"dev"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !auth.ValidDev(in.Dev) {
		fail(w, 400, "bad dev", "dev 必须是 12 位小写十六进制的 MAC")
		return
	}

	ip := rateKeyIP(r)
	if a.regDev.exceeded(in.Dev) || a.regIP.exceeded(ip) {
		fail(w, 429, "rate limited", "")
		return
	}
	a.regDev.hit(in.Dev)
	a.regIP.hit(ip)

	token, keyID, err := auth.IssueDeviceKey(a.store, "", in.Dev, "", true)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	// 同一台设备反复重注册时，旧的待配对密钥作废，别越堆越多。
	revoked, err := a.store.DisablePendingDeviceKeysExcept(in.Dev, keyID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	for _, id := range revoked {
		a.v.Invalidate(id)
	}

	now := time.Now()
	code, expires, err := a.issuePairCode(in.Dev, keyID, now)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]any{
		"token": token, "code": code, "expires_at": expires,
		"now": now.Unix(), "pending": true,
	})
}

// handleDevicePairCode：POST /api/device/pair-code（device 鉴权）
//
// 电视/LCD 上的码过期后，设备用当前待配对密钥换一个新码。
func (a *API) handleDevicePairCode(w http.ResponseWriter, r *http.Request, id auth.Identity) {
	if !auth.ValidDev(id.Dev) {
		fail(w, 400, "bad dev", "")
		return
	}
	if !a.pairCodeDev.allow(id.Dev) {
		fail(w, 429, "rate limited", "")
		return
	}
	k, ok, err := a.store.KeyByID(id.KeyID)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	if !ok || k.Role != string(auth.RoleDevice) || k.Dev != id.Dev ||
		k.Disabled || !k.Pending {
		fail(w, 409, "already paired", "")
		return
	}
	now := time.Now()
	code, expires, err := a.issuePairCode(id.Dev, id.KeyID, now)
	if err != nil {
		fail(w, 500, "server error", "")
		return
	}
	writeJSON(w, 200, map[string]any{
		"code": code, "expires_at": expires, "now": now.Unix(),
	})
}
