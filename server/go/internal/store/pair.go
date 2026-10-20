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

// RestorePairCode 在配对失败但码还没被正确主人使用时放回去。
//
// 先消费再验归属能防并发双成功；但别人输错码不该把原主人的码烧掉。
// ON CONFLICT(dev) DO NOTHING：期间设备若已经换了新码，不覆盖新的。
func (s *Store) RestorePairCode(c PairCode) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(
		`INSERT INTO pair_codes(`+pairCols+`) VALUES(?,?,?,?,?)
		 ON CONFLICT(dev) DO NOTHING`,
		c.Dev, c.CodeHash, c.KeyID, c.Expires, c.Created)
	return err
}

// ConsumePairCode 原子地取出并删除一个未过期的码。
//
// 配对开始时必须先消费再验归属：两个请求同时带同一个有效码时，
// DELETE ... RETURNING 只有一个能拿到行，另一个拿不到，避免建出两个
// 用户、签出两把 agent 令牌。
func (s *Store) ConsumePairCode(hash string, at int64) (PairCode, bool, error) {
	return scanPairCode(s.db.QueryRow(
		`DELETE FROM pair_codes WHERE code_hash=? AND expires>=?
		 RETURNING `+pairCols, hash, at))
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

// PairRedeem 是网页配对生成的一次性兑换凭据（只存链接哈希）。
type PairRedeem struct {
	LinkHash string
	Dev      string
	KeyID    string
	Expires  int64
	Created  int64
}

func (s *Store) PutPairRedeem(r PairRedeem) error {
	if r.Created == 0 {
		r.Created = now()
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	// 顺手清过期行，表不会堆积。
	if _, err := s.db.Exec(`DELETE FROM pair_redeems WHERE expires < ?`, r.Created); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO pair_redeems(link_hash,dev,key_id,expires,created) VALUES(?,?,?,?,?)`,
		r.LinkHash, r.Dev, r.KeyID, r.Expires, r.Created)
	return err
}

// ConsumePairRedeem 原子地取出并删除一条未过期的兑换凭据：同一链接并发兑换只有一个拿到行。
func (s *Store) ConsumePairRedeem(hash string, at int64) (PairRedeem, bool, error) {
	var r PairRedeem
	err := s.db.QueryRow(
		`DELETE FROM pair_redeems WHERE link_hash=? AND expires>=?
		 RETURNING link_hash,dev,key_id,expires,created`, hash, at).
		Scan(&r.LinkHash, &r.Dev, &r.KeyID, &r.Expires, &r.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return PairRedeem{}, false, nil
	}
	return r, err == nil, err
}
