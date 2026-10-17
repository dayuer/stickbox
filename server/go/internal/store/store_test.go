package store

import (
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
