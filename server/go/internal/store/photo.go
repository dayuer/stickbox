package store

import (
	"database/sql"
	"errors"
)

// ErrPhotoQuota 表示用户照片张数或总字节超过硬配额。
var ErrPhotoQuota = errors.New("store: photo quota exceeded")

// Photo 是缩放、剥元数据后的最终 JPEG 的元数据。
type Photo struct {
	SHA256  string
	Size    int64
	Width   int
	Height  int
	Created int64
}

// AddPhoto 在配额内登记一张照片。同一 sha256 已存在时 created=false。
// 配额检查与插入在同一把写锁内，避免并发上传把配额顶穿。
func (s *Store) AddPhoto(userID string, p Photo, maxCount int, maxBytes int64) (
	created bool, count int, bytes int64, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()

	var exists int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM photos WHERE user_id=? AND sha256=?`,
		userID, p.SHA256).Scan(&exists); err != nil {
		return false, 0, 0, err
	}
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(size),0) FROM photos WHERE user_id=?`,
		userID).Scan(&count, &bytes); err != nil {
		return false, 0, 0, err
	}
	if exists > 0 {
		return false, count, bytes, nil
	}
	if count >= maxCount || bytes+p.Size > maxBytes {
		return false, count, bytes, ErrPhotoQuota
	}
	_, err = s.db.Exec(
		`INSERT INTO photos(user_id, sha256, size, width, height, created)
		 VALUES(?,?,?,?,?,?)`,
		userID, p.SHA256, p.Size, p.Width, p.Height, p.Created)
	if err != nil {
		return false, count, bytes, err
	}
	return true, count + 1, bytes + p.Size, nil
}

func (s *Store) PhotoBySHA(userID, sha string) (Photo, bool, error) {
	var p Photo
	err := s.db.QueryRow(
		`SELECT sha256, size, width, height, created
		 FROM photos WHERE user_id=? AND sha256=?`, userID, sha).
		Scan(&p.SHA256, &p.Size, &p.Width, &p.Height, &p.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return Photo{}, false, nil
	}
	return p, err == nil, err
}

// PhotosOfUser 按上传时间倒序返回，供 agent photo ls。
func (s *Store) PhotosOfUser(userID string) ([]Photo, error) {
	rows, err := s.db.Query(
		`SELECT sha256, size, width, height, created
		 FROM photos WHERE user_id=? ORDER BY created DESC, sha256`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Photo{}
	for rows.Next() {
		var p Photo
		if err := rows.Scan(&p.SHA256, &p.Size, &p.Width, &p.Height, &p.Created); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePhoto 删除照片元数据；调用方负责删文件和清单项（P1 后续）。
func (s *Store) DeletePhoto(userID, sha string) (bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	res, err := s.db.Exec(`DELETE FROM photos WHERE user_id=? AND sha256=?`, userID, sha)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PlaylistEntry 是清单的一行；pos 从 0 连续。
type PlaylistEntry struct {
	Pos       int
	Kind      string
	Theme     string
	SHA256    string
	DurationS int
}

// AppendPhotoToPlaylist 把照片追加到清单末尾（已在清单里则不动）。
// 清单第一次出现时补一条 pos=0 的 board。返回当前 plv 和是否在清单里。
func (s *Store) AppendPhotoToPlaylist(userID, sha, boardTheme string, durationS int) (
	plv int64, in bool, err error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO playlist_meta(user_id, plv, updated) VALUES(?,0,?)`,
		userID, now()); err != nil {
		return 0, false, err
	}
	var pos0 int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM playlist_entries WHERE user_id=? AND pos=0`,
		userID).Scan(&pos0); err != nil {
		return 0, false, err
	}
	if pos0 == 0 {
		if _, err := tx.Exec(
			`INSERT INTO playlist_entries(user_id,pos,kind,theme,sha256,duration_s)
			 VALUES(?,0,'board',?,'',20)`, userID, boardTheme); err != nil {
			return 0, false, err
		}
	}

	var found int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM playlist_entries
		 WHERE user_id=? AND kind='photo' AND sha256=?`, userID, sha).Scan(&found); err != nil {
		return 0, false, err
	}
	if found > 0 {
		if err := tx.QueryRow(
			`SELECT plv FROM playlist_meta WHERE user_id=?`, userID).Scan(&plv); err != nil {
			return 0, false, err
		}
		return plv, true, tx.Commit()
	}

	var maxPos sql.NullInt64
	if err := tx.QueryRow(
		`SELECT MAX(pos) FROM playlist_entries WHERE user_id=?`, userID).Scan(&maxPos); err != nil {
		return 0, false, err
	}
	pos := 1
	if maxPos.Valid {
		pos = int(maxPos.Int64) + 1
	}
	if _, err := tx.Exec(
		`INSERT INTO playlist_entries(user_id,pos,kind,theme,sha256,duration_s)
		 VALUES(?,?,'photo','',?,?)`, userID, pos, sha, durationS); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(
		`UPDATE playlist_meta SET plv=plv+1, updated=? WHERE user_id=?`, now(), userID); err != nil {
		return 0, false, err
	}
	if err := tx.QueryRow(
		`SELECT plv FROM playlist_meta WHERE user_id=?`, userID).Scan(&plv); err != nil {
		return 0, false, err
	}
	return plv, true, tx.Commit()
}

// PlaylistVersion 返回当前清单版本（0 = 还没有清单）。
func (s *Store) PlaylistVersion(userID string) (int64, error) {
	var plv int64
	err := s.db.QueryRow(`SELECT plv FROM playlist_meta WHERE user_id=?`, userID).Scan(&plv)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return plv, err
}

// PlaylistEntries 按 pos 升序返回完整清单。
func (s *Store) PlaylistEntries(userID string) ([]PlaylistEntry, error) {
	rows, err := s.db.Query(
		`SELECT pos, kind, theme, sha256, duration_s
		 FROM playlist_entries WHERE user_id=? ORDER BY pos`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlaylistEntry{}
	for rows.Next() {
		var e PlaylistEntry
		if err := rows.Scan(&e.Pos, &e.Kind, &e.Theme, &e.SHA256, &e.DurationS); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
