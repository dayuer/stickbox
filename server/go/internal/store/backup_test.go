package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestHealthAndBackup(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := st.Health(ctx); err != nil {
		t.Fatalf("Health: %v", err)
	}

	u, err := st.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"five_hour":{"used_percentage":12.5,"resets_at":1762223000}}`)
	if err := st.PutUsage(u.ID, body, 1762219400); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "backups", "jobs.db")
	if err := st.Backup(ctx, out); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// 备份是一份可独立打开的完整库。
	bst, err := Open(out)
	if err != nil {
		t.Fatalf("打开备份: %v", err)
	}
	defer bst.Close()
	got, ts, ok, err := bst.GetUsage(u.ID)
	if err != nil || !ok {
		t.Fatalf("备份里读不到用量: ok=%v err=%v", ok, err)
	}
	if string(got) != string(body) || ts != 1762219400 {
		t.Errorf("备份内容 = %s ts=%d", got, ts)
	}

	// 目标已存在时拒绝覆盖，避免 cron 重跑把好备份冲掉。
	if err := st.Backup(ctx, out); err == nil {
		t.Error("覆盖已有备份应当失败")
	}
}
