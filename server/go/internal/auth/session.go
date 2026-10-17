package auth

import "github.com/dayuer/stickbox/server/go/internal/store"

// SessionStore 是签发与吊销需要的 store 能力。
type SessionStore interface {
	InsertKey(k store.Key) error
	DisableKey(keyID string) error
	DisableKeysOfDevice(dev, role string) ([]string, error)
	DisableDeviceKeysExcept(dev, keepKeyID string) ([]string, error)
}

// IssueAgentKey 为用户的一台电脑签发 agent 令牌。
// 不设过期，靠 logout / 吊销下线；name 是电脑名。
func IssueAgentKey(v *Verifier, st SessionStore, userID, name string) (string, error) {
	token, _, err := issueKey(v, st, store.Key{
		Role: string(RoleAgent), UserID: userID, Name: name,
	})
	return token, err
}

// IssueDeviceKey 为一台屏签发 device 密钥。
//
// pending=true 时密钥还没完成配对：它属于 dev，但 user_id 为空，
// 不能拿到任何用量，只能收配对通知。配对成功后由
// store.ActivateDeviceKey 转正。
func IssueDeviceKey(v *Verifier, st SessionStore, userID, dev, name string, pending bool) (token, keyID string, err error) {
	return issueKey(v, st, store.Key{
		Dev: dev, Role: string(RoleDevice), UserID: userID, Name: name,
		Pending: pending,
	})
}

func issueKey(v *Verifier, st SessionStore, k store.Key) (token, keyID string, err error) {
	keyID, secret, token := NewToken()
	hash, err := v.HashSecret(keyID, secret)
	if err != nil {
		return "", "", err
	}
	k.KeyID, k.Hash = keyID, hash
	if err := st.InsertKey(k); err != nil {
		return "", "", err
	}
	return token, keyID, nil
}

// RevokeSession 吊销一把。必须同时清校验缓存，否则它还能再活一小时。
func RevokeSession(st SessionStore, v *Verifier, keyID string) error {
	if err := st.DisableKey(keyID); err != nil {
		return err
	}
	v.Invalidate(keyID)
	return nil
}

// RevokeDeviceKeys 吊销某台屏的全部 device 密钥。解绑走这里。
func RevokeDeviceKeys(st SessionStore, v *Verifier, dev string) error {
	ids, err := st.DisableKeysOfDevice(dev, string(RoleDevice))
	if err != nil {
		return err
	}
	for _, id := range ids {
		v.Invalidate(id)
	}
	return nil
}

// RevokeDeviceKeysExcept 配对转正时吊销旧主人的密钥，保留刚转正的那把。
func RevokeDeviceKeysExcept(st SessionStore, v *Verifier, dev, keepKeyID string) error {
	ids, err := st.DisableDeviceKeysExcept(dev, keepKeyID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		v.Invalidate(id)
	}
	return nil
}
