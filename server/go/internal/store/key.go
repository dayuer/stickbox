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
}

const keyCols = `key_id,dev,role,user_id,name,key_hash,created,last_seen,disabled`

func (s *Store) InsertKey(k Key) error {
	if k.Created == 0 {
		k.Created = now()
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO devices(`+keyCols+`) VALUES(?,?,?,?,?,?,?,0,0)`,
		k.KeyID, k.Dev, k.Role, k.UserID, k.Name, k.Hash, k.Created)
	return err
}

func (s *Store) KeyByID(keyID string) (Key, bool, error) {
	var k Key
	var disabled int
	err := s.db.QueryRow(`SELECT `+keyCols+` FROM devices WHERE key_id=?`, keyID).
		Scan(&k.KeyID, &k.Dev, &k.Role, &k.UserID, &k.Name, &k.Hash,
			&k.Created, &k.LastSeen, &disabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Key{}, false, nil
		}
		return Key{}, false, err
	}
	k.Disabled = disabled != 0
	return k, true, nil
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
		var disabled int
		if err := rows.Scan(&k.KeyID, &k.Dev, &k.Role, &k.UserID, &k.Name, &k.Hash,
			&k.Created, &k.LastSeen, &disabled); err != nil {
			return nil, err
		}
		k.Disabled = disabled != 0
		out = append(out, k)
	}
	return out, rows.Err()
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

// DisableKeysOfUser 吊销某用户某角色的全部密钥，返回被吊销的 key_id
// ——调用方要拿它去清 Verifier 的缓存，否则最多还能再活一小时。
func (s *Store) DisableKeysOfUser(userID, role string) ([]string, error) {
	return s.disableKeysWhere(`user_id=? AND role=? AND disabled=0`, userID, role)
}

func (s *Store) DisableKeysOfDevice(dev, role string) ([]string, error) {
	return s.disableKeysWhere(`dev=? AND role=? AND disabled=0`, dev, role)
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

// DevicesOfUser 返回该用户名下所有 dev，去重。ACL 判定要用它。
func (s *Store) DevicesOfUser(userID string) ([]string, error) {
	if userID == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT dev FROM devices WHERE user_id=? AND disabled=0 AND dev<>''`,
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

// OwnerOfDevice 是抢绑防护的基础：这台设备当前属于谁。
// ClaimKey 把一把还没绑定设备的密钥绑到 dev 上。
//
// 设备首次连 MQTT 时调用：username 就是它自己的 dev，服务端从那里学到 MAC——
// **不需要 App 先去问设备要**。让 App 去读 MAC 正是当初逼出「配网要连两次
// 热点」的那个多余步骤。
//
// 条件里的 dev=” 是并发保护：两台设备拿同一把密钥同时连，只有一台能绑上。
func (s *Store) ClaimKey(keyID, dev string) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(
		`UPDATE devices SET dev=?, last_seen=? WHERE key_id=? AND dev='' AND disabled=0`,
		dev, now(), keyID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeviceMeta 是一台设备在 devices 表里的展示信息。
//
// name 和 bound 取自那把**还有效**的密钥：重新 enroll 会吊销旧密钥、插一条新的，
// 所以取 disabled=0 的那条才是当前的名字和绑定时间。
type DeviceMeta struct {
	Name  string
	Bound int64
	Seen  int64
}

// DeviceMeta 查一台设备的展示信息。第二个返回值为 false 表示没有有效密钥。
func (s *Store) DeviceMeta(dev string) (DeviceMeta, bool, error) {
	var m DeviceMeta
	err := s.db.QueryRow(
		`SELECT name, created, last_seen FROM devices
		  WHERE dev=? AND role='device' AND disabled=0
		  ORDER BY created DESC LIMIT 1`, dev).Scan(&m.Name, &m.Bound, &m.Seen)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceMeta{}, false, nil
	}
	if err != nil {
		return DeviceMeta{}, false, err
	}
	return m, true, nil
}

func (s *Store) OwnerOfDevice(dev string) (string, bool, error) {
	var userID string
	err := s.db.QueryRow(
		`SELECT user_id FROM devices
		 WHERE dev=? AND role='device' AND disabled=0 AND user_id<>'' LIMIT 1`,
		dev).Scan(&userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return userID, true, nil
}
