package store

import (
	"database/sql"
	"errors"
)

// PutSales 覆盖该用户的最新经营数据。和 usage 一样只留最新一份：
// 屏幕只看当前值，历史曲线以后真要做再加表。
func (s *Store) PutSales(userID string, body []byte, received int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO sales(user_id, body, received) VALUES(?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET body=excluded.body, received=excluded.received`,
		userID, string(body), received)
	return err
}

func (s *Store) GetSales(userID string) ([]byte, int64, bool, error) {
	var body string
	var received int64
	err := s.db.QueryRow(`SELECT body, received FROM sales WHERE user_id=?`, userID).
		Scan(&body, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	return []byte(body), received, true, nil
}
