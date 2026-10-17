package store

import "testing"

func TestPairCodeRoundTrip(t *testing.T) {
	s := open(t)
	c := PairCode{Dev: "d1", CodeHash: "h1", KeyID: "k1", Expires: 1000}
	if err := s.PutPairCode(c); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.PairCodeByHash("h1")
	if err != nil || !ok || got.Dev != "d1" || got.KeyID != "k1" {
		t.Fatalf("按码取回 %+v ok=%v err=%v", got, ok, err)
	}
	byDev, ok, _ := s.PairCodeForDev("d1")
	if !ok || byDev.CodeHash != "h1" {
		t.Errorf("按 dev 取回 %+v ok=%v", byDev, ok)
	}

	// 同一台设备重新出码：覆盖旧行。
	if err := s.PutPairCode(PairCode{Dev: "d1", CodeHash: "h2", KeyID: "k2", Expires: 2000}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PairCodeByHash("h1"); ok {
		t.Error("旧码还能查到")
	}
	if got, ok, _ := s.PairCodeForDev("d1"); !ok || got.CodeHash != "h2" {
		t.Errorf("重新出码后 = %+v ok=%v", got, ok)
	}
}

// code_hash 唯一：两个设备撞同一个码必须失败，调用方换码重试。
func TestPairCodeHashUnique(t *testing.T) {
	s := open(t)
	if err := s.PutPairCode(PairCode{Dev: "d1", CodeHash: "same", KeyID: "k1", Expires: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPairCode(PairCode{Dev: "d2", CodeHash: "same", KeyID: "k2", Expires: 1}); err == nil {
		t.Error("两台设备用同一个 code_hash 竟然写成功了")
	}
}

func TestDeletePairCode(t *testing.T) {
	s := open(t)
	s.PutPairCode(PairCode{Dev: "d1", CodeHash: "h", KeyID: "k", Expires: 100})
	if err := s.DeletePairCode("d1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PairCodeForDev("d1"); ok {
		t.Error("删除后仍存在")
	}
}

func TestDeleteExpiredPairCodes(t *testing.T) {
	s := open(t)
	s.PutPairCode(PairCode{Dev: "old", CodeHash: "h1", KeyID: "k1", Expires: 100})
	s.PutPairCode(PairCode{Dev: "new", CodeHash: "h2", KeyID: "k2", Expires: 900})
	if err := s.DeleteExpiredPairCodes(500); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PairCodeForDev("old"); ok {
		t.Error("过期码没删掉")
	}
	if _, ok, _ := s.PairCodeForDev("new"); !ok {
		t.Error("未过期码被误删")
	}
}
