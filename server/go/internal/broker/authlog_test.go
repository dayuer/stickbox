package broker

import (
	"path/filepath"
	"testing"
	"time"

	mqtt "github.com/mochi-mqtt/server/v2"
	"github.com/mochi-mqtt/server/v2/packets"

	"github.com/dayuer/stickbox/server/go/internal/auth"
	"github.com/dayuer/stickbox/server/go/internal/store"
)

func TestAuthLogRingRecent(t *testing.T) {
	l := newAuthLog(3)
	for i := 0; i < 5; i++ {
		l.add(AuthEvent{At: time.Unix(int64(i), 0), Dev: "d", Code: i})
	}
	l.add(AuthEvent{At: time.Unix(9, 0), Dev: "other", Code: 9})

	got := l.recent("d", 2)
	if len(got) != 2 || got[0].Code != 4 || got[1].Code != 3 {
		t.Fatalf("recent(d,2) = %+v", got)
	}
	all := l.recent("", 10)
	if len(all) != 3 || all[0].Dev != "other" {
		t.Fatalf("recent(all) = %+v", all)
	}
}

func TestConnectedKeys(t *testing.T) {
	h := &Hook{ids: newIDTable()}
	cl1, cl2, cl3 := &mqtt.Client{}, &mqtt.Client{}, &mqtt.Client{}
	h.ids.put(cl1, auth.Identity{Dev: "d", Role: auth.RoleDevice, KeyID: "k1"})
	h.ids.put(cl2, auth.Identity{Dev: "d", Role: auth.RoleDevice, KeyID: "k2"})
	h.ids.put(cl3, auth.Identity{Dev: "other", Role: auth.RoleDevice, KeyID: "k3"})

	got := h.connectedKeys("d")
	if len(got) != 2 || got[0] != "k1" || got[1] != "k2" {
		t.Fatalf("connectedKeys = %v", got)
	}
}

func TestOnConnectRecordsDenied(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := &Hook{
		v:    auth.NewVerifier(st, "test-pepper-0123456789abcdef"),
		ids:  newIDTable(),
		auth: newAuthLog(10),
	}
	const dev = "f412fa87c9e0"
	cl := &mqtt.Client{Net: mqtt.ClientConnection{Remote: "192.0.2.1:1234"}}
	pk := packets.Packet{Connect: packets.ConnectParams{
		Username: []byte(dev), Password: []byte("unknown.secret"),
	}}
	if err := h.OnConnect(cl, pk); err != nil {
		t.Fatalf("OnConnect = %v，期望 nil（交给 0x05）", err)
	}
	evs := h.auth.recent(dev, 1)
	if len(evs) != 1 || evs[0].Code != 0x05 || evs[0].Result != "invalid_key" {
		t.Fatalf("event = %+v", evs)
	}
}
