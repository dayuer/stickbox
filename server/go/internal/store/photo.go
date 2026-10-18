package store

import (
	"database/sql"
	"errors"
	"strings"
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

// Playlist 返回清单版本与条目。plv=0 表示还没有清单。
func (s *Store) Playlist(userID string) (int64, []PlaylistEntry, error) {
	plv, err := s.PlaylistVersion(userID)
	if err != nil {
		return 0, nil, err
	}
	entries, err := s.PlaylistEntries(userID)
	return plv, entries, err
}

// ReplacePlaylist 用完整期望状态替换清单，pos 按数组顺序重排，plv 递增一次。
func (s *Store) ReplacePlaylist(userID string, entries []PlaylistEntry) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := replacePlaylistTx(tx, userID, entries); err != nil {
		return 0, err
	}
	plv, err := bumpPlvTx(tx, userID)
	if err != nil {
		return 0, err
	}
	return plv, tx.Commit()
}

// BumpPlaylistVersion 让清单版本前进一次（初始化 token 等“清单文档变了”
// 但条目没变的场景用）。
func (s *Store) BumpPlaylistVersion(userID string) (int64, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	plv, err := bumpPlvTx(tx, userID)
	if err != nil {
		return 0, err
	}
	return plv, tx.Commit()
}

func bumpPlvTx(tx *sql.Tx, userID string) (int64, error) {
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO playlist_meta(user_id, plv, updated) VALUES(?,0,?)`,
		userID, now()); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(
		`UPDATE playlist_meta SET plv=plv+1, updated=? WHERE user_id=?`, now(), userID); err != nil {
		return 0, err
	}
	var plv int64
	err := tx.QueryRow(`SELECT plv FROM playlist_meta WHERE user_id=?`, userID).Scan(&plv)
	return plv, err
}

func replacePlaylistTx(tx *sql.Tx, userID string, entries []PlaylistEntry) error {
	if _, err := tx.Exec(`DELETE FROM playlist_entries WHERE user_id=?`, userID); err != nil {
		return err
	}
	for i, e := range entries {
		if _, err := tx.Exec(
			`INSERT INTO playlist_entries(user_id,pos,kind,theme,sha256,duration_s)
			 VALUES(?,?,?,?,?,?)`,
			userID, i, e.Kind, e.Theme, e.SHA256, e.DurationS); err != nil {
			return err
		}
	}
	return nil
}

// DeletePhotoWithPlaylist 删除照片元数据与所有清单引用，并把剩余条目重排成
// 0..N-1。返回新 plv 与是否真的删到了照片。
func (s *Store) DeletePhotoWithPlaylist(userID, sha string) (int64, bool, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM photos WHERE user_id=? AND sha256=?`, userID, sha)
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, false, nil // defer Rollback；没有变化就不动 plv
	}

	rows, err := tx.Query(
		`SELECT pos, kind, theme, sha256, duration_s FROM playlist_entries
		 WHERE user_id=? AND NOT (kind='photo' AND sha256=?) ORDER BY pos`,
		userID, sha)
	if err != nil {
		return 0, false, err
	}
	var keep []PlaylistEntry
	for rows.Next() {
		var e PlaylistEntry
		if err := rows.Scan(&e.Pos, &e.Kind, &e.Theme, &e.SHA256, &e.DurationS); err != nil {
			rows.Close()
			return 0, false, err
		}
		keep = append(keep, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if err := replacePlaylistTx(tx, userID, keep); err != nil {
		return 0, false, err
	}
	plv, err := bumpPlvTx(tx, userID)
	if err != nil {
		return 0, false, err
	}
	return plv, true, tx.Commit()
}

// —— 一次性 SD 初始化授权 ——

type PhotoInit struct {
	Token   string
	Force   bool
	Expires int64
}

func (s *Store) SetPhotoInit(userID string, in PhotoInit) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	force := 0
	if in.Force {
		force = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO photo_init(user_id, token, force, expires) VALUES(?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET token=excluded.token,
		   force=excluded.force, expires=excluded.expires`,
		userID, in.Token, force, in.Expires)
	return err
}

func (s *Store) PhotoInit(userID string) (PhotoInit, bool, error) {
	var in PhotoInit
	var force int
	err := s.db.QueryRow(
		`SELECT token, force, expires FROM photo_init WHERE user_id=?`, userID).
		Scan(&in.Token, &force, &in.Expires)
	if errors.Is(err, sql.ErrNoRows) {
		return PhotoInit{}, false, nil
	}
	in.Force = force != 0
	return in, err == nil, err
}

func (s *Store) ClearPhotoInit(userID string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`DELETE FROM photo_init WHERE user_id=?`, userID)
	return err
}

// —— 设备照片能力/状态 ——

type DevicePhoto struct {
	UserID   string
	Cap      bool
	Ready    bool
	PLV      int64
	Have     int
	Bad      []string
	FreeKB   int64
	State    string
	InitDone string
	Updated  int64
}

func (s *Store) PutDevicePhoto(dev string, p DevicePhoto) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	cap, ready := 0, 0
	if p.Cap {
		cap = 1
	}
	if p.Ready {
		ready = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO device_photo(dev,user_id,cap,ready,plv,have,bad,free_kb,state,init_done,updated)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(dev) DO UPDATE SET user_id=excluded.user_id, cap=excluded.cap,
		   ready=excluded.ready, plv=excluded.plv, have=excluded.have, bad=excluded.bad,
		   free_kb=excluded.free_kb, state=excluded.state, init_done=excluded.init_done,
		   updated=excluded.updated`,
		dev, p.UserID, cap, ready, p.PLV, p.Have, strings.Join(p.Bad, ","),
		p.FreeKB, p.State, p.InitDone, p.Updated)
	return err
}

func (s *Store) DevicePhoto(dev string) (DevicePhoto, bool, error) {
	var p DevicePhoto
	var cap, ready int
	var bad string
	err := s.db.QueryRow(
		`SELECT user_id, cap, ready, plv, have, bad, free_kb, state, init_done, updated
		 FROM device_photo WHERE dev=?`, dev).
		Scan(&p.UserID, &cap, &ready, &p.PLV, &p.Have, &bad,
			&p.FreeKB, &p.State, &p.InitDone, &p.Updated)
	if errors.Is(err, sql.ErrNoRows) {
		return DevicePhoto{}, false, nil
	}
	if err != nil {
		return DevicePhoto{}, false, err
	}
	p.Cap, p.Ready = cap != 0, ready != 0
	if bad != "" {
		p.Bad = strings.Split(bad, ",")
	}
	return p, true, nil
}

// InitDoneForUser 判断该用户任一设备是否用掉了这个 token 前缀。
func (s *Store) InitDoneForUser(userID, prefix string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM device_photo WHERE user_id=? AND init_done=?`,
		userID, prefix).Scan(&n)
	return n > 0, err
}
