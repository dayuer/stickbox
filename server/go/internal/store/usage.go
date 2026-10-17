package store

import (
	"database/sql"
	"errors"
)

// PutUsage 覆盖该用户的最新用量。只留最新一份：屏幕和 App 都只看当前值，
// 历史曲线以后真要做再加表。
func (s *Store) PutUsage(userID string, body []byte, received int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO usage(user_id, body, received) VALUES(?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET body=excluded.body, received=excluded.received`,
		userID, string(body), received)
	return err
}

func (s *Store) GetUsage(userID string) ([]byte, int64, bool, error) {
	var body string
	var received int64
	err := s.db.QueryRow(`SELECT body, received FROM usage WHERE user_id=?`, userID).
		Scan(&body, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	return []byte(body), received, true, nil
}

// KeysOfUser 列出该用户某个角色下仍有效的密钥。App 管理 agent 时用。
func (s *Store) KeysOfUser(userID, role string) ([]Key, error) {
	rows, err := s.db.Query(
		`SELECT `+keyCols+` FROM devices WHERE user_id=? AND role=? AND disabled=0
		 ORDER BY created`, userID, role)
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
