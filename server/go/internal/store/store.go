// Package store 是用户、设备密钥、配对码与用量的唯一真相来源。
//
// T7 起没有手机号和 App：用户由配对动作创建，设备靠配对码归属到用户。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

func now() int64 { return time.Now().Unix() }

type rowScanner interface{ Scan(...any) error }

type Store struct {
	db  *sql.DB
	wmu sync.Mutex // 写串行化：写入量极低，一把锁足够
}

const schema = `
CREATE TABLE IF NOT EXISTS devices(
  key_id    TEXT PRIMARY KEY,
  dev       TEXT NOT NULL,
  role      TEXT NOT NULL,
  user_id   TEXT NOT NULL DEFAULT '',
  name      TEXT NOT NULL DEFAULT '',
  key_hash  TEXT NOT NULL,
  created   INTEGER NOT NULL DEFAULT 0,
  last_seen INTEGER NOT NULL DEFAULT 0,
  disabled  INTEGER NOT NULL DEFAULT 0,
  pending   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS devices_dev  ON devices(dev);
CREATE INDEX IF NOT EXISTS devices_user ON devices(user_id);

CREATE TABLE IF NOT EXISTS users(
  id       TEXT PRIMARY KEY,
  created  INTEGER NOT NULL DEFAULT 0,
  disabled INTEGER NOT NULL DEFAULT 0
);

-- 配对码：每台设备最多一条。code_hash 唯一，避免两个设备同时出同一个码。
CREATE TABLE IF NOT EXISTS pair_codes(
  dev        TEXT PRIMARY KEY,
  code_hash  TEXT NOT NULL UNIQUE,
  key_id     TEXT NOT NULL,
  expires    INTEGER NOT NULL DEFAULT 0,
  created    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS pair_codes_expires ON pair_codes(expires);

-- Claude 订阅用量，每个用户只留最新一份。body 是 usage.Report 的 JSON。
CREATE TABLE IF NOT EXISTS usage(
  user_id  TEXT PRIMARY KEY,
  body     TEXT NOT NULL,
  received INTEGER NOT NULL DEFAULT 0
);

-- 当日经营数据（第二路数据源），同样每个用户只留最新一份。
-- body 是 sales.Report 的 JSON，自带所属本地日期；跨零点算不算今天由
-- 数据源判断，不在这里过期，否则「更新 X 前」就没的说了。
CREATE TABLE IF NOT EXISTS sales(
  user_id  TEXT PRIMARY KEY,
  body     TEXT NOT NULL,
  received INTEGER NOT NULL DEFAULT 0
);
`

// 打印退役时一次性清掉的旧表。
const dropPrintTables = `
DROP TABLE IF EXISTS jobs;
DROP TABLE IF EXISTS test_runs;
DROP TABLE IF EXISTS printers;
DROP TABLE IF EXISTS profiles;
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite",
		path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(dropPrintTables); err != nil {
		db.Close()
		return nil, err
	}
	// 旧库升级：Python 版没有这两列。
	db.Exec(`ALTER TABLE devices ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE devices ADD COLUMN pending INTEGER NOT NULL DEFAULT 0`)

	// T7 去掉了手机号：旧 users 表有 phone_hmac NOT NULL，必须先重建，
	// 否则新建用户插不进去。user_phones / sms_codes 一并清掉。
	if err := migrateUsers(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`
		DROP TABLE IF EXISTS user_phones;
		DROP TABLE IF EXISTS sms_codes;`); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrateUsers(db *sql.DB) error {
	has, err := hasColumn(db, "users", "phone_hmac")
	if err != nil || !has {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE users_new(
		   id       TEXT PRIMARY KEY,
		   created  INTEGER NOT NULL DEFAULT 0,
		   disabled INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO users_new(id, created, disabled)
		 SELECT id, created, disabled FROM users`,
		`DROP TABLE users`,
		`ALTER TABLE users_new RENAME TO users`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	var n int
	q := fmt.Sprintf(
		`SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name=?`, table)
	if err := db.QueryRow(q, column).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Health 只做一次最小的读，确认数据库文件可读。健康检查用，不暴露细节。
func (s *Store) Health(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

// Backup 用 SQLite 的 VACUUM INTO 做在线一致性备份。
//
// WAL 模式下直接 cp jobs.db 会漏掉 -wal 里的数据；VACUUM INTO 由 SQLite
// 自己生成一份完整快照，服务运行中也可以跑。目标文件必须不存在。
func (s *Store) Backup(ctx context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("store: 备份路径为空")
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("store: 备份文件已存在: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(path, "'", "''")
	_, err := s.db.ExecContext(ctx, "VACUUM INTO '"+quoted+"'")
	return err
}
