package ops

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestLoadSmokeOffline(t *testing.T) {
	dir := t.TempDir()
	rep, err := RunLoadSmoke(context.Background(), LoadOptions{
		Concurrency: 3,
		Sequential:  4,
		Timeout:     2 * time.Minute,
		Dir:         dir,
	})
	if err != nil {
		t.Fatalf("load smoke: %v", err)
	}
	if !rep.Pass {
		t.Fatalf("load smoke failed: errors=%v checks=%v", rep.Errors, rep.Checks)
	}
	if rep.ErrorCount != 0 {
		t.Fatalf("error count = %d (%v)", rep.ErrorCount, rep.Errors)
	}
	if rep.ExpectedRuns != 7 || rep.CompletedRuns != 7 || rep.CreatedRuns != 7 {
		t.Fatalf("run counts = expected %d created %d completed %d", rep.ExpectedRuns, rep.CreatedRuns, rep.CompletedRuns)
	}
	if rep.DuplicateRuns != 0 || rep.DuplicateResources != 0 || rep.DuplicateIdentities != 0 || rep.DuplicateOperationKey != 0 {
		t.Fatalf("duplicates detected: %+v", rep)
	}
	if !rep.IdempotentReplaySameID {
		t.Fatal("idempotent replay returned a different run id")
	}
	if rep.CreateLatency.Count != 7 || rep.RunLatency.Count != 7 {
		t.Fatalf("latency samples: create=%+v run=%+v", rep.CreateLatency, rep.RunLatency)
	}
	if rep.CreateLatency.P95MS < rep.CreateLatency.P50MS || rep.RunLatency.P95MS < rep.RunLatency.P50MS {
		t.Fatalf("percentiles are not monotonic: create=%+v run=%+v", rep.CreateLatency, rep.RunLatency)
	}
	if _, err := os.Stat(rep.DBPath); err != nil {
		t.Fatalf("evidence database missing: %v", err)
	}
}

func TestComputePercentiles(t *testing.T) {
	p := computePercentiles(nil)
	if p.Count != 0 || p.P50MS != 0 {
		t.Fatalf("empty percentiles = %+v", p)
	}
	// 1ms..100ms: nearest-rank P50=50, P95=95, P99=99, max=100.
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	p = computePercentiles(d)
	if p.Count != 100 || p.P50MS != 50 || p.P95MS != 95 || p.P99MS != 99 || p.MaxMS != 100 {
		t.Fatalf("percentiles = %+v", p)
	}
}
