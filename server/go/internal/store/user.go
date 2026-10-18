package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
)

// User 没有手机号、没有密码：它只是「一台电脑（agent）名下的屏」的归属。
type User struct {
	ID       string
	Created  int64
	Disabled bool
	// TZ 是用户级时区设置（IANA 名或 Etc/GMT±N），空表示未设置。
	// 生效优先级：用户设置 > agent 上报 > 服务端 default_tz。
	TZ string
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败时继续运行没有意义
	}
	return hex.EncodeToString(b)
}

const userCols = `id,created,disabled,tz`

// CreateUser 在配对成功时创建用户。
func (s *Store) CreateUser() (User, error) {
	u := User{ID: newID(), Created: now()}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO users(`+userCols+`) VALUES(?,?,0,'')`,
		u.ID, u.Created)
	return u, err
}

// SetUserTZ 设置或清除（tz 为空）用户级时区。
// found=false 表示用户不存在。
func (s *Store) SetUserTZ(id, tz string) (found bool, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(`UPDATE users SET tz=? WHERE id=?`, tz, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UserTZ 只取用户设置的时区；用户不存在或未设置时返回空串。
func (s *Store) UserTZ(id string) (string, error) {
	var tz string
	err := s.db.QueryRow(`SELECT tz FROM users WHERE id=?`, id).Scan(&tz)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return tz, err
}

func (s *Store) UserByID(id string) (User, bool, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id))
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.ID, &u.Created, &disabled, &u.TZ); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

func scanUser(r rowScanner) (User, bool, error) {
	var u User
	var disabled int
	err := r.Scan(&u.ID, &u.Created, &disabled, &u.TZ)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, false, nil
		}
		return User{}, false, err
	}
	u.Disabled = disabled != 0
	return u, true, nil
}
