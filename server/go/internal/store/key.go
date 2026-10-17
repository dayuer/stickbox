package store

import (
	"database/sql"
	"errors"
)

type Key struct {
	KeyID    string
	Dev      string
	Role     string
	UserID   string
	Name     string
	Hash     string
	Created  int64
	LastSeen int64
	Disabled bool
	// Pending 表示这把 device 密钥已经签发、但还没完成配对。
	// 待配对密钥可以连 MQTT 收配对通知，但不属于任何用户。
	Pending bool
}

const keyCols = `key_id,dev,role,user_id,name,key_hash,created,last_seen,disabled,pending`

func (s *Store) InsertKey(k Key) error {
	if k.Created == 0 {
		k.Created = now()
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO devices(`+keyCols+`) VALUES(?,?,?,?,?,?,?,0,?,?)`,
		k.KeyID, k.Dev, k.Role, k.UserID, k.Name, k.Hash, k.Created,
		b2i(k.Disabled), b2i(k.Pending))
	return err
}

func (s *Store) KeyByID(keyID string) (Key, bool, error) {
	return scanKey(s.db.QueryRow(
		`SELECT `+keyCols+` FROM devices WHERE key_id=?`, keyID))
}

func (s *Store) ListKeys() ([]Key, error) {
	rows, err := s.db.Query(`SELECT ` + keyCols + ` FROM devices ORDER BY dev, created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Key{}
	for rows.Next() {
		var k Key
		var disabled, pending int
		if err := rows.Scan(&k.KeyID, &k.Dev, &k.Role, &k.UserID, &k.Name, &k.Hash,
			&k.Created, &k.LastSeen, &disabled, &pending); err != nil {
			return nil, err
		}
		k.Disabled = disabled != 0
		k.Pending = pending != 0
		out = append(out, k)
	}
	return out, rows.Err()
}

func scanKey(r rowScanner) (Key, bool, error) {
	var k Key
	var disabled, pending int
	err := r.Scan(&k.KeyID, &k.Dev, &k.Role, &k.UserID, &k.Name, &k.Hash,
		&k.Created, &k.LastSeen, &disabled, &pending)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Key{}, false, nil
		}
		return Key{}, false, err
	}
	k.Disabled = disabled != 0
	k.Pending = pending != 0
	return k, true, nil
}

func (s *Store) DisableKey(keyID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE devices SET disabled=1 WHERE key_id=?`, keyID)
	return err
}

func (s *Store) TouchKey(keyID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`UPDATE devices SET last_seen=? WHERE key_id=?`, now(), keyID)
	return err
}

// ActivateDeviceKey 把待配对密钥转正：绑定用户、清掉 pending。
func (s *Store) ActivateDeviceKey(keyID, userID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`UPDATE devices SET user_id=?, pending=0 WHERE key_id=? AND disabled=0`,
		userID, keyID)
	return err
}

func (s *Store) DisableKeysOfDevice(dev, role string) ([]string, error) {
	return s.disableKeysWhere(`dev=? AND role=? AND disabled=0`, dev, role)
}

// DisableDeviceKeysExcept 吊销这台设备上除 keepKeyID 以外的全部 device 密钥。
// 配对转正时用：旧主人的密钥作废，刚转正的那把留着。
func (s *Store) DisableDeviceKeysExcept(dev, keepKeyID string) ([]string, error) {
	return s.disableKeysWhere(
		`dev=? AND role='device' AND disabled=0 AND key_id<>?`, dev, keepKeyID)
}

// DisablePendingDeviceKeysExcept 清掉同设备之前没配对的密钥。
// 设备反复重注册时不能让待配对密钥越堆越多。
func (s *Store) DisablePendingDeviceKeysExcept(dev, keepKeyID string) ([]string, error) {
	return s.disableKeysWhere(
		`dev=? AND role='device' AND disabled=0 AND pending=1 AND key_id<>?`,
		dev, keepKeyID)
}

func (s *Store) disableKeysWhere(where string, args ...any) ([]string, error) {
	rows, err := s.db.Query(`SELECT key_id FROM devices WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	for _, id := range ids {
		if _, err := s.db.Exec(`UPDATE devices SET disabled=1 WHERE key_id=?`, id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// DevicesOfUser 返回该用户名下所有已配对的 dev，去重。
func (s *Store) DevicesOfUser(userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT dev FROM devices
		 WHERE user_id=? AND disabled=0 AND pending=0 AND dev<>''`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceMeta 是一台设备在 devices 表里的展示信息。
type DeviceMeta struct {
	Name  string
	Bound int64
	Seen  int64
}

// DeviceMeta 查一台设备的展示信息，只看已配对的有效密钥。
func (s *Store) DeviceMeta(dev string) (DeviceMeta, bool, error) {
	var m DeviceMeta
	err := s.db.QueryRow(
		`SELECT name, created, last_seen FROM devices
		  WHERE dev=? AND role='device' AND disabled=0 AND pending=0
		  ORDER BY created DESC LIMIT 1`, dev).Scan(&m.Name, &m.Bound, &m.Seen)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceMeta{}, false, nil
	}
	if err != nil {
		return DeviceMeta{}, false, err
	}
	return m, true, nil
}

// OwnerOfDevice 返回这台设备当前已配对的主人。待配对密钥不算。
func (s *Store) OwnerOfDevice(dev string) (string, bool, error) {
	var userID string
	err := s.db.QueryRow(
		`SELECT user_id FROM devices
		 WHERE dev=? AND role='device' AND disabled=0 AND pending=0
		   AND user_id<>'' LIMIT 1`,
		dev).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return userID, true, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
