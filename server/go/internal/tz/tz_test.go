package tz

import "testing"

func TestNormalizeIANA(t *testing.T) {
	for _, in := range []string{"Asia/Jakarta", "Asia/Shanghai", "America/New_York", "UTC"} {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q) = %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("Normalize(%q) = %q", in, got)
		}
	}
}

// "+07:00" 这类固定偏移要转成 Etc/GMT：IANA 的符号与 UTC 相反。
func TestNormalizeFixedOffset(t *testing.T) {
	cases := map[string]string{
		"+07:00": "Etc/GMT-7",
		"-05:00": "Etc/GMT+5",
		"+00:00": "Etc/GMT+0",
		"+14:00": "Etc/GMT-14",
		"-12:00": "Etc/GMT+12",
	}
	for in, want := range cases {
		got, err := Normalize(in)
		if err != nil {
			t.Errorf("Normalize(%q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	for _, in := range []string{
		"", "  ", "Local", "Mars/Olympus", "Asia/", "../etc/passwd",
		"+7:00", "+07:60", "+24:00", "+07:30", "07:00", "Etc/GMT+15",
	} {
		if got, err := Normalize(in); err == nil {
			t.Errorf("Normalize(%q) = %q，期望报错", in, got)
		}
	}
}

func TestResolvePriority(t *testing.T) {
	cases := []struct {
		name, user, agent, def string
		wantTZ, wantSrc        string
	}{
		{"用户优先", "Asia/Jakarta", "Asia/Tokyo", "UTC", "Asia/Jakarta", SourceUser},
		{"用户为空回退 agent", "", "Asia/Tokyo", "UTC", "Asia/Tokyo", SourceAgent},
		{"都为空回退默认", "", "", "Asia/Bangkok", "Asia/Bangkok", SourceDefault},
		{"非法用户值被跳过", "Bad/Zone", "Asia/Tokyo", "UTC", "Asia/Tokyo", SourceAgent},
		{"非法 agent 值被跳过", "", "Bad/Zone", "Asia/Bangkok", "Asia/Bangkok", SourceDefault},
		{"默认也非法则 UTC", "", "", "Bad/Zone", "UTC", SourceDefault},
		{"默认缺省则 UTC", "", "", "", "UTC", SourceDefault},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, src := Resolve(c.user, c.agent, c.def)
			if got != c.wantTZ || src != c.wantSrc {
				t.Fatalf("Resolve(%q,%q,%q) = %q,%q；期望 %q,%q",
					c.user, c.agent, c.def, got, src, c.wantTZ, c.wantSrc)
			}
		})
	}
}

func TestResolveNormalizesFixedOffsetAgentValue(t *testing.T) {
	got, src := Resolve("", "+07:00", "UTC")
	if got != "Etc/GMT-7" || src != SourceAgent {
		t.Fatalf("Resolve = %q,%q", got, src)
	}
}
