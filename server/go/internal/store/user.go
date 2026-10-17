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
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand 失败时继续运行没有意义
	}
	return hex.EncodeToString(b)
}

const userCols = `id,created,disabled`

// CreateUser 在配对成功时创建用户。
func (s *Store) CreateUser() (User, error) {
	u := User{ID: newID(), Created: now()}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`INSERT INTO users(`+userCols+`) VALUES(?,?,0)`,
		u.ID, u.Created)
	return u, err
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
		if err := rows.Scan(&u.ID, &u.Created, &disabled); err != nil {
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
	err := r.Scan(&u.ID, &u.Created, &disabled)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, false, nil
		}
		return User{}, false, err
	}
	u.Disabled = disabled != 0
	return u, true, nil
}
