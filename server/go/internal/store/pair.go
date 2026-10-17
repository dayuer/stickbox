package store

import (
	"database/sql"
	"errors"
)

// PairCode 是一台设备当前待使用的配对码。每台设备最多一条。
type PairCode struct {
	Dev      string
	CodeHash string
	KeyID    string
	Expires  int64
	Created  int64
}

const pairCols = `dev,code_hash,key_id,expires,created`

// PutPairCode 写入或替换该设备的配对码。
// code_hash 有唯一约束：撞上别的设备的码会返回错误，调用方换一个码重试。
func (s *Store) PutPairCode(c PairCode) error {
	if c.Created == 0 {
		c.Created = now()
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO pair_codes(`+pairCols+`) VALUES(?,?,?,?,?)
		 ON CONFLICT(dev) DO UPDATE SET
		   code_hash=excluded.code_hash, key_id=excluded.key_id,
		   expires=excluded.expires, created=excluded.created`,
		c.Dev, c.CodeHash, c.KeyID, c.Expires, c.Created)
	return err
}

func (s *Store) PairCodeByHash(hash string) (PairCode, bool, error) {
	return scanPairCode(s.db.QueryRow(
		`SELECT `+pairCols+` FROM pair_codes WHERE code_hash=?`, hash))
}

func (s *Store) PairCodeForDev(dev string) (PairCode, bool, error) {
	return scanPairCode(s.db.QueryRow(
		`SELECT `+pairCols+` FROM pair_codes WHERE dev=?`, dev))
}

func scanPairCode(r rowScanner) (PairCode, bool, error) {
	var c PairCode
	err := r.Scan(&c.Dev, &c.CodeHash, &c.KeyID, &c.Expires, &c.Created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PairCode{}, false, nil
		}
		return PairCode{}, false, err
	}
	return c, true, nil
}

// DeletePairCode 消耗或作废一台设备的配对码。
func (s *Store) DeletePairCode(dev string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM pair_codes WHERE dev=?`, dev)
	return err
}

// DeleteExpiredPairCodes 清掉过期码，避免唯一索引里堆垃圾。
func (s *Store) DeleteExpiredPairCodes(at int64) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM pair_codes WHERE expires < ?`, at)
	return err
}
