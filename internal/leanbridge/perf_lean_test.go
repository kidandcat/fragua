//go:build lean

package leanbridge

import (
	"fmt"
	"math/big"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func BenchmarkLeanSegSeg(b *testing.B) {
	b.ReportAllocs()
	if _, err := leanSegSeg(0, 0, 1, 0, 0, 1, 1, 1); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := leanSegSeg(0, 0, 5_000_000, 0, 0, 250_000, 8_000_000, 250_000); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLeanSegSegBatch(b *testing.B) {
	b.ReportAllocs()
	segs := make([][8]int64, 256)
	for i := range segs {
		segs[i] = [8]int64{0, 0, 5_000_000, int64(i), 0, 250_000, 8_000_000, 250_000}
	}
	if _, err := leanSegSegBatch(segs); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := leanSegSegBatch(segs); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLeanHotLoop(b *testing.B) {
	b.ReportAllocs()
	const chunk = 4096
	if _, err := leanSegSum(1); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s, err := leanSegSum(chunk)
		if err != nil {
			b.Fatal(err)
		}
		if s.Sign() < 0 {
			b.Fatal(s)
		}
	}
}

// TestSegSumAgrees checks the in-Lean loop against the Go loop, including a
// size where hoisting the distance out of the loop would still return a
// plausible small answer. The timed ratio is a backstop: 20000 iterations
// that actually call the geometry cannot finish in a handful of frame times.
func TestSegSumAgrees(t *testing.T) {
	for _, n := range []int{0, 1, 7, 100, 1000} {
		g := pureSegSum(n)
		l, err := leanSegSum(n)
		if err != nil {
			t.Fatal(err)
		}
		if g.Cmp(l) != 0 {
			t.Fatalf("n=%d go %s lean %s", n, g, l)
		}
	}
	one, err := leanSegSum(1)
	if err != nil {
		t.Fatal(err)
	}
	if pureSegSum(1).Cmp(one) != 0 {
		t.Fatal(one)
	}
	warm, err := leanSegSum(1)
	if err != nil {
		t.Fatal(err)
	}
	_ = warm
	t1 := time.Duration(0)
	for i := 0; i < 5; i++ {
		s := time.Now()
		if _, err := leanSegSum(1); err != nil {
			t.Fatal(err)
		}
		t1 += time.Since(s)
	}
	t1 /= 5
	s := time.Now()
	got, err := leanSegSum(20000)
	if err != nil {
		t.Fatal(err)
	}
	tN := time.Since(s)
	if pureSegSum(20000).Cmp(got) != 0 {
		t.Fatalf("n=20000 mismatch %s vs %s", got, pureSegSum(20000))
	}
	if tN < t1*100 {
		t.Fatalf("in-Lean loop looks hoisted: n=1 %s, n=20000 %s", t1, tN)
	}
	t.Logf("seg_sum n=1 %s, n=20000 %s (%.0f×)", t1, tN, float64(tN)/float64(t1))
}

// TestHotLoopPerf measures calls/sec and resident memory for three granularities
// of the same segment-distance loop. It logs; it does not fail on a time budget.
func TestHotLoopPerf(t *testing.T) {
	const n = 20000
	if pureSegSum(n).Cmp(mustSum(t, n)) != 0 {
		t.Fatal("checksum")
	}

	runtime.GC()
	goRSS0, goHWM0 := procKB(os.Getpid())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	tGo := time.Now()
	acc := pureSegSum(n)
	goDt := time.Since(tGo)
	runtime.ReadMemStats(&after)
	goRSS1, goHWM1 := procKB(os.Getpid())
	if acc.Sign() < 0 {
		t.Fatal(acc)
	}

	pid := HelperPID()
	if pid == 0 {
		t.Fatal("helper pid")
	}
	idleRSS, idleHWM := procKB(pid)

	// In-Lean loop first, so its high-water mark is not the per-call storm.
	tIn := time.Now()
	if _, err := leanSegSum(n); err != nil {
		t.Fatal(err)
	}
	inDt := time.Since(tIn)
	inRSS, inHWM := procKB(pid)
	goInRSS, _ := procKB(os.Getpid())

	tBr := time.Now()
	for i := 0; i < n; i++ {
		k := int64(i)
		if _, err := leanSegSeg(k, 0, k+5_000_000, int64(i%17), 0, 250_000, 8_000_000, 250_000+int64(i%5)); err != nil {
			t.Fatal(err)
		}
	}
	brDt := time.Since(tBr)
	brRSS, brHWM := procKB(pid)
	goBrRSS, goBrHWM := procKB(os.Getpid())

	report := fmt.Sprintf(
		"n=%d go_loop %s (%.0f/s) heap_alloc %d→%d B total_alloc %+d B rss %d→%d KiB hwm %d→%d KiB; "+
			"lean_in_loop %s (%.0f/s) helper_rss idle %d → %d KiB hwm %d→%d KiB go_rss %d KiB; "+
			"lean_per_call %s (%.0f/s) helper_rss %d KiB hwm %d KiB go_rss %d→%d KiB hwm %d KiB",
		n,
		goDt, rate(n, goDt), before.HeapAlloc, after.HeapAlloc, int64(after.TotalAlloc)-int64(before.TotalAlloc),
		goRSS0, goRSS1, goHWM0, goHWM1,
		inDt, rate(n, inDt), idleRSS, inRSS, idleHWM, inHWM, goInRSS,
		brDt, rate(n, brDt), brRSS, brHWM, goRSS1, goBrRSS, goBrHWM,
	)
	t.Log(report)
}

func mustSum(t *testing.T, n int) *big.Int {
	t.Helper()
	s, err := leanSegSum(n)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rate(n int, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / d.Seconds()
}

func procKB(pid int) (rss, hwm uint64) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		var p *uint64
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			p = &rss
		case strings.HasPrefix(line, "VmHWM:"):
			p = &hwm
		default:
			continue
		}
		fs := strings.Fields(line)
		if len(fs) >= 2 {
			v, err := strconv.ParseUint(fs[1], 10, 64)
			if err == nil {
				*p = v
			}
		}
	}
	return rss, hwm
}
