package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/config"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

func TestRunUserTZRemote(t *testing.T) {
	var gotUser, gotTZ string
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user-tz" || r.Method != http.MethodPost {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		calls++
		var in struct {
			UserID string `json:"user_id"`
			TZ     string `json:"tz"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		gotUser, gotTZ = in.UserID, in.TZ
		w.Write([]byte(`{"ok":1}`))
	}))
	defer srv.Close()

	cfg := &config.Config{AdminAddr: strings.TrimPrefix(srv.URL, "http://")}
	if err := runUserTZ(cfg, []string{"u1", "+07:00"}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || gotUser != "u1" || gotTZ != "Etc/GMT-7" {
		t.Fatalf("请求 = calls:%d user:%q tz:%q", calls, gotUser, gotTZ)
	}

	if err := runUserTZ(cfg, []string{"u1"}); err != nil {
		t.Fatal(err)
	}
	if gotTZ != "" {
		t.Fatalf("清除请求 tz = %q", gotTZ)
	}
}

// 服务端没运行：退回直接写库，不丢设置。
func TestRunUserTZFallbackToDB(t *testing.T) {
	cfg := &config.Config{Root: t.TempDir(), AdminAddr: "127.0.0.1:1"}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := runUserTZ(cfg, []string{u.ID, "Asia/Jakarta"}); err != nil {
		t.Fatalf("runUserTZ: %v", err)
	}

	st, err = store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, ok, err := st.UserByID(u.ID)
	if err != nil || !ok || got.TZ != "Asia/Jakarta" {
		t.Fatalf("库里的用户 = %+v ok=%v err=%v", got, ok, err)
	}
}

// admin 有响应但拒绝（例如用户不存在）时，不应再退回直接写库。
func TestRunUserTZServerRejectsNoFallback(t *testing.T) {
	cfg := &config.Config{Root: t.TempDir()}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg.AdminAddr = strings.TrimPrefix(srv.URL, "http://")

	if err := runUserTZ(cfg, []string{u.ID, "Asia/Jakarta"}); err == nil {
		t.Fatal("服务端拒绝时不应成功")
	}
	st, err = store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, _, _ := st.UserByID(u.ID)
	if got.TZ != "" {
		t.Fatalf("服务端拒绝后仍写库了：%q", got.TZ)
	}
}

func TestRunUserTZRejectsInvalid(t *testing.T) {
	cfg := &config.Config{Root: t.TempDir(), AdminAddr: "127.0.0.1:1"}
	for _, args := range [][]string{
		{},
		{"u1", "Mars/Olympus"},
		{"u1", "Local"},
		{"u1", "UTC", "extra"},
	} {
		if err := runUserTZ(cfg, args); err == nil {
			t.Errorf("args=%v 应报错", args)
		}
	}
}
