// Package store 是账号、设备密钥与用量的唯一真相来源。
//
// 打印退役后这里不再有作业队列；设备在线状态是内存里的心跳表（见 registry），
// 不需要持久化。
package store

import (
	"database/sql"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

func now() int64 { return time.Now().Unix() }

type rowScanner interface{ Scan(...any) error }

type Store struct {
	db  *sql.DB
	wmu sync.Mutex // 写串行化：百台量级写入量极低，一把锁足够，省掉 SQLITE_BUSY 重试
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
  disabled  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS devices_dev  ON devices(dev);
CREATE INDEX IF NOT EXISTS devices_user ON devices(user_id);

CREATE TABLE IF NOT EXISTS users(
  id         TEXT PRIMARY KEY,
  phone_hmac TEXT NOT NULL UNIQUE,
  phone_tail TEXT NOT NULL DEFAULT '',
  created    INTEGER NOT NULL DEFAULT 0,
  last_login INTEGER NOT NULL DEFAULT 0,
  disabled   INTEGER NOT NULL DEFAULT 0
);

-- 完整号码单独成表：登录和 ACL 是高频路径，个人信息不进热表
CREATE TABLE IF NOT EXISTS user_phones(
  user_id   TEXT PRIMARY KEY,
  phone_enc BLOB NOT NULL,
  updated   INTEGER NOT NULL DEFAULT 0
);

-- Claude 订阅用量，每个用户只留最新一份。body 是 usage.Report 的 JSON。
-- 由用户电脑上的 stickbox-agent 上报（role=agent 的密钥）。
CREATE TABLE IF NOT EXISTS usage(
  user_id  TEXT PRIMARY KEY,
  body     TEXT NOT NULL,
  received INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS sms_codes(
  phone_hmac TEXT PRIMARY KEY,
  code_hash  TEXT NOT NULL,
  expires    INTEGER NOT NULL DEFAULT 0,
  attempts   INTEGER NOT NULL DEFAULT 0,
  sent_at    INTEGER NOT NULL DEFAULT 0,
  day_start  INTEGER NOT NULL DEFAULT 0,
  day_count  INTEGER NOT NULL DEFAULT 0
);
`

// 打印退役时一次性清掉的旧表。打印功能已经整条删除，里面的数据没有消费者。
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
	// 旧库升级：Python 版没有这一列。已存在时报错，忽略即可。
	db.Exec(`ALTER TABLE devices ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
