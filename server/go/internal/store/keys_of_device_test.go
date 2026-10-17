package store_test

import (
	"path/filepath"
	"testing"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

func TestKeysOfDevice(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	v := auth.NewVerifier(st, "0123456789abcdefghijklmn")
	if _, _, err := auth.IssueDeviceKey(v, st, u.ID, "f412fa87c9e0", "active", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.IssueDeviceKey(v, st, "", "f412fa87c9e0", "pending", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := auth.IssueDeviceKey(v, st, u.ID, "aabbccddeeff", "other", false); err != nil {
		t.Fatal(err)
	}

	keys, err := st.KeysOfDevice("f412fa87c9e0")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", keys)
	}
	states := map[string]string{}
	for _, k := range keys {
		switch {
		case k.Pending:
			states[k.Name] = "pending"
		case k.Disabled:
			states[k.Name] = "disabled"
		default:
			states[k.Name] = "active"
		}
	}
	if states["active"] != "active" || states["pending"] != "pending" {
		t.Errorf("states = %v", states)
	}
}
