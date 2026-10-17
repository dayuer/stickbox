package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ {
		s, err := Open(p)
		if err != nil {
			t.Fatalf("第 %d 次 Open: %v", i+1, err)
		}
		s.Close()
	}
}

// 旧库的 users 表有 phone_hmac NOT NULL，T7 必须重建它，
// 否则新用户插不进去；老用户的 id/created 要保留。
func TestMigrateLegacyUsersTable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE users(
		   id TEXT PRIMARY KEY, phone_hmac TEXT NOT NULL UNIQUE,
		   phone_tail TEXT, created INTEGER, last_login INTEGER, disabled INTEGER)`,
		`INSERT INTO users VALUES('u1','hmac','8888',123,456,0)`,
		`CREATE TABLE user_phones(user_id TEXT PRIMARY KEY, phone_enc BLOB, updated INTEGER)`,
		`CREATE TABLE sms_codes(phone_hmac TEXT PRIMARY KEY)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	u, ok, err := s.UserByID("u1")
	if err != nil || !ok || u.Created != 123 {
		t.Fatalf("迁移后用户 = %+v ok=%v err=%v", u, ok, err)
	}
	// 新用户不再需要手机号字段。
	if _, err := s.CreateUser(); err != nil {
		t.Fatalf("迁移后仍插不进新用户：%v", err)
	}
	for _, table := range []string{"user_phones", "sms_codes"} {
		var n int
		s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if n != 0 {
			t.Errorf("旧表 %s 没删掉", table)
		}
	}
}

// 用量每用户只留最新一份。
func TestUsageOverwritesLatest(t *testing.T) {
	s := open(t)
	if err := s.PutUsage("u1", []byte(`{"a":1}`), 100); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUsage("u1", []byte(`{"a":2}`), 200); err != nil {
		t.Fatal(err)
	}
	body, at, ok, err := s.GetUsage("u1")
	if err != nil || !ok || string(body) != `{"a":2}` || at != 200 {
		t.Fatalf("取回 %s at=%d ok=%v err=%v", body, at, ok, err)
	}
}

func TestUsageMissing(t *testing.T) {
	s := open(t)
	if _, _, ok, err := s.GetUsage("nobody"); ok || err != nil {
		t.Errorf("ok=%v err=%v，期望查不到", ok, err)
	}
}
