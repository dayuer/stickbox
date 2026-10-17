//go:build unix

package auth

import (
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dayuer/stickbox/server/go/internal/store"
)

// TestVerifyLatency2000 对比两种方案在「2000 台设备同时重连」时的表现：
//   - no-cache：生产方案，每次 CONNECT 查一次主键 + HMAC。
//   - cache-recheck：命中缓存也查主键（否则 CLI 吊销看不到），再比对缓存
//     里的 secret 哈希。
//
// 两种方案的 DB 查询次数相同，差别只在缓存 map 的锁和 HMAC 计算。
func TestVerifyLatency2000(t *testing.T) {
	if testing.Short() {
		t.Skip("2000 设备校验耗时基准")
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "lat.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, err := st.CreateUser()
	if err != nil {
		t.Fatal(err)
	}

	const n = 2000
	tokens := make([]string, n)
	seedVerifier := NewVerifier(st, testPepper)
	for i := range tokens {
		dev := fmt.Sprintf("%012x", i+1)
		tok, _, err := IssueDeviceKey(seedVerifier, st, u.ID, dev, "", false)
		if err != nil {
			t.Fatalf("签发第 %d 把密钥：%v", i, err)
		}
		tokens[i] = tok
	}

	for _, tc := range []struct {
		name string
		v    *Verifier
	}{
		{"no-cache", NewVerifier(st, testPepper)},
		{"cache-recheck", NewVerifierCached(st, testPepper)},
	} {
		// 缓存方案先暖一遍，模拟服务稳定运行后的重连风暴。
		if tc.name != "no-cache" {
			for _, tok := range tokens {
				if _, err := tc.v.Verify(tok); err != nil {
					t.Fatal(err)
				}
			}
		}

		lat := make([]time.Duration, n)
		var wg sync.WaitGroup
		var failures int64
		cpu0 := procCPU()
		start := time.Now()
		for i := range tokens {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				t0 := time.Now()
				if _, err := tc.v.Verify(tokens[i]); err != nil {
					atomic.AddInt64(&failures, 1)
				}
				lat[i] = time.Since(t0)
			}(i)
		}
		wg.Wait()
		wall := time.Since(start)
		cpu := procCPU() - cpu0
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p := func(q float64) time.Duration {
			if len(lat) == 0 {
				return 0
			}
			i := int(float64(len(lat)-1) * q)
			return lat[i]
		}
		cpuPct := 0.0
		if wall > 0 {
			cpuPct = float64(cpu) / float64(wall) * 100
		}
		t.Logf("%s: n=%d failures=%d wall=%s cpu=%s (%.0f%%) p50=%s p95=%s p99=%s max=%s",
			tc.name, n, failures, wall.Round(time.Millisecond), cpu.Round(time.Millisecond), cpuPct,
			p(0.50), p(0.95), p(0.99), lat[len(lat)-1])
	}
}

func procCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Sec)*time.Second + time.Duration(ru.Utime.Usec)*time.Microsecond +
		time.Duration(ru.Stime.Sec)*time.Second + time.Duration(ru.Stime.Usec)*time.Microsecond
}
