// Package auth 管密钥的生成、校验、会话与 ACL 判定。
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 令牌 secret 是 24 字节 crypto/rand，不是低熵密码：离线爆破不可行。
// 因此线上格式用 HMAC-SHA256(pepper, secret)，既快又不给重连风暴
// 制造 64MB 的内存峰值。
//
// argon2id 只保留用于校验旧记录：成功校验一次后就地迁移成 HMAC。
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MB
	argonThreads = 4
	argonKeyLen  = 32
	saltLen      = 16

	hmacScheme = "hmac-sha256"
	hmacVer    = "v=1"
)

var ErrBadToken = errors.New("auth: 令牌格式非法")

// NewToken 生成一把新密钥，返回 (keyID, secret, 完整令牌)。
// 明文只在签发时出现一次，不落盘。
//
// 令牌格式 {key_id}.{secret}：校验时按 key_id 直接定位到唯一一行，
// 不遍历该设备的所有密钥逐个校验——那会让开销随密钥数线性增长。
func NewToken() (keyID, secret, token string) {
	idb := make([]byte, 6)
	sb := make([]byte, 24)
	if _, err := rand.Read(idb); err != nil {
		panic(err) // crypto/rand 失败时继续运行没有意义
	}
	if _, err := rand.Read(sb); err != nil {
		panic(err)
	}
	keyID = hex.EncodeToString(idb)                   // 12 字符
	secret = base64.RawURLEncoding.EncodeToString(sb) // 32 字符
	return keyID, secret, keyID + "." + secret
}

func SplitToken(token string) (keyID, secret string, err error) {
	i := strings.IndexByte(token, '.')
	if i <= 0 || i == len(token)-1 {
		return "", "", ErrBadToken
	}
	keyID, secret = token[:i], token[i+1:]
	if strings.ContainsRune(secret, '.') {
		return "", "", ErrBadToken
	}
	return keyID, secret, nil
}

// HashSecret 生成服务端存储的令牌哈希：HMAC-SHA256(pepper, secret)。
//
// 不需要 salt：secret 本身是 24 字节随机数，彩虹表和预计算无从下手。
// pepper 让数据库单独泄露时哈希不可离线验证。
func HashSecret(pepper, secret string) (string, error) {
	if pepper == "" {
		return "", errors.New("auth: 缺少 token pepper")
	}
	mac := hmac.New(sha256.New, []byte(pepper))
	mac.Write([]byte(secret))
	sum := mac.Sum(nil)
	return "$" + hmacScheme + "$" + hmacVer + "$" +
		base64.RawStdEncoding.EncodeToString(sum), nil
}

// VerifySecret 校验 secret 与存储的哈希。legacy 为 true 表示命中的是
// 旧的 argon2id 记录，调用方可就地迁移成 HMAC。
func VerifySecret(pepper, secret, encoded string) (ok, legacy bool) {
	parts := strings.Split(encoded, "$")
	switch {
	case len(parts) == 4 && parts[1] == hmacScheme && parts[2] == hmacVer:
		want, err := base64.RawStdEncoding.DecodeString(parts[3])
		if err != nil {
			return false, false
		}
		mac := hmac.New(sha256.New, []byte(pepper))
		mac.Write([]byte(secret))
		return subtle.ConstantTimeCompare(mac.Sum(nil), want) == 1, false
	case len(parts) == 6 && parts[1] == "argon2id":
		return verifyArgon2(secret, encoded), true
	default:
		return false, false
	}
}

// hashArgon2 只用于测试旧记录迁移路径；生产签发不再走 argon2。
func hashArgon2(secret string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum)), nil
}

// verifyArgon2 只用于旧记录。参数来自记录本身，迁移后不再使用。
func verifyArgon2(secret, encoded string) bool {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	var m uint32
	var tt, p int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &tt, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(secret), salt, uint32(tt), m, uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}
