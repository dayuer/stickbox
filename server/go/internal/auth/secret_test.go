package auth

import (
	"strings"
	"testing"
)

func TestNewTokenFormat(t *testing.T) {
	keyID, secret, token := NewToken()
	if len(keyID) != 12 {
		t.Errorf("keyID 长度 %d，期望 12", len(keyID))
	}
	if len(secret) < 32 {
		t.Errorf("secret 长度 %d，太短", len(secret))
	}
	if token != keyID+"."+secret {
		t.Errorf("token = %q，期望 keyID.secret", token)
	}
	if _, _, tok2 := NewToken(); tok2 == token {
		t.Error("两次生成的令牌相同——随机源有问题")
	}
}

func TestSplitToken(t *testing.T) {
	id, sec, err := SplitToken("abc123.deadbeef")
	if err != nil || id != "abc123" || sec != "deadbeef" {
		t.Errorf("id=%q sec=%q err=%v", id, sec, err)
	}
	for _, bad := range []string{"", "nodot", ".onlysecret", "onlyid.", "a.b.c"} {
		if _, _, err := SplitToken(bad); err == nil {
			t.Errorf("SplitToken(%q) 应当报错", bad)
		}
	}
}

func TestHashVerifyRoundTrip(t *testing.T) {
	h, err := HashSecret(testPepper, "k1", "s3cret-value")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$hmac-sha256$v=1$") {
		t.Errorf("哈希格式 = %q，期望 HMAC 编码串", h)
	}
	if ok, legacy := VerifySecret(testPepper, "k1", "s3cret-value", h); !ok || legacy {
		t.Errorf("正确密钥校验失败：ok=%v legacy=%v", ok, legacy)
	}
	if ok, _ := VerifySecret(testPepper, "k1", "wrong", h); ok {
		t.Error("错误密钥竟然通过")
	}
	if ok, _ := VerifySecret(testPepper, "k1", "s3cret-value", "$hmac-sha256$v=1$garbage"); ok {
		t.Error("畸形哈希串竟然通过")
	}
	if ok, _ := VerifySecret("other-pepper-0123456", "k1", "s3cret-value", h); ok {
		t.Error("换 pepper 后仍然通过")
	}
}

// 哈希绑定 key_id：把同一 secret 的哈希挪到别的 key_id 下不能通过。
func TestHashBindsKeyID(t *testing.T) {
	h, err := HashSecret(testPepper, "k1", "s3cret-value")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifySecret(testPepper, "k2", "s3cret-value", h); ok {
		t.Fatal("哈希在另一个 key_id 下仍然通过")
	}
	if ok, _ := VerifySecret(testPepper, "k1", "s3cret-value", h); !ok {
		t.Fatal("正确 key_id 下校验失败")
	}
}

func TestEmptyPepperRejected(t *testing.T) {
	if _, err := HashSecret("", "k1", "s"); err == nil {
		t.Fatal("空 pepper 不应能生成哈希")
	}
	h, _ := HashSecret(testPepper, "k1", "s")
	if ok, _ := VerifySecret("", "k1", "s", h); ok {
		t.Fatal("空 pepper 不应能校验")
	}
}

func TestLegacyArgon2AcceptedAndMarked(t *testing.T) {
	h, err := hashArgon2("s3cret-value")
	if err != nil {
		t.Fatal(err)
	}
	ok, legacy := VerifySecret(testPepper, "k1", "s3cret-value", h)
	if !ok || !legacy {
		t.Fatalf("旧 argon2 记录应通过并标记 legacy：ok=%v legacy=%v", ok, legacy)
	}
	if ok, _ := VerifySecret(testPepper, "k1", "wrong", h); ok {
		t.Error("旧记录的错误密钥竟然通过")
	}
}
