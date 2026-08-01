package bench

import (
	"testing"
	"time"
)

// blocksAt builds points for one second: n blocks stamped at ts, txs each.
func blocksAt(startHeight int64, ts time.Time, n, txs int) []blockPoint {
	points := make([]blockPoint, 0, n)
	for i := range n {
		points = append(points, blockPoint{Height: startHeight + int64(i), Txs: txs, At: ts})
	}
	return points
}

// TestBucketTPSIsBlockRateIndependent is the regression this metric exists for.
// The previous 5-block sliding window divided a window's transactions by the
// difference of two second-granularity block timestamps. A chain producing 15
// blocks/s fits a whole window inside one second, so the divisor was 0 (rate
// reported as 0) or a full second for ~0.3s of work (~3x understated) — which
// made allegro read ~6k against tempo's ~12.5k when it was doing ~25k.
//
// Two chains doing the same 24,000 tx/s must report the same rate whether that
// arrives as 3 fat blocks or 15 thin ones.
func TestBucketTPSIsBlockRateIndependent(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)

	for _, tc := range []struct {
		name             string
		blocksPerSec     int
		txsPerBlock      int
		firstBlockHeight int64
	}{
		{"slow chain (tempo-like)", 3, 8000, 100},
		{"fast chain (allegro-like)", 15, 1600, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var points []blockPoint
			height := tc.firstBlockHeight
			for sec := range 3 {
				points = append(points, blocksAt(height, base.Add(time.Duration(sec)*time.Second), tc.blocksPerSec, tc.txsPerBlock)...)
				height += int64(tc.blocksPerSec)
			}

			buckets := bucketTPS(points)
			if len(buckets) != 3 {
				t.Fatalf("active seconds = %d, want 3", len(buckets))
			}
			const want = 24000
			for _, b := range buckets {
				if b.Txs != want {
					t.Errorf("second %s = %d tx/s, want %d", b.At, b.Txs, want)
				}
			}
			if got := sustainedTPS(buckets); got != want {
				t.Errorf("sustained = %.0f, want %d", got, want)
			}
			if got := medianTPS(buckets); got != want {
				t.Errorf("median = %.0f, want %d", got, want)
			}
		})
	}
}

// TestBucketTPSExcludesIdleEdges keeps the ramp-up and the idle tail out of the
// rate: DumpBlockStats walks from height 2 and stops only after num_idle empty
// blocks, so both ends are padded with empties that are not part of the load.
func TestBucketTPSExcludesIdleEdges(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	var points []blockPoint
	points = append(points, blocksAt(1, base, 4, 0)...)                      // pre-load
	points = append(points, blocksAt(5, base.Add(time.Second), 2, 500)...)   // load
	points = append(points, blocksAt(7, base.Add(2*time.Second), 2, 300)...) // load
	points = append(points, blocksAt(9, base.Add(3*time.Second), 5, 0)...)   // idle tail

	buckets := bucketTPS(points)
	if len(buckets) != 2 {
		t.Fatalf("active seconds = %d, want 2 (idle edges dropped)", len(buckets))
	}
	if buckets[0].Txs != 1000 || buckets[1].Txs != 600 {
		t.Errorf("bucket rates = %d, %d; want 1000, 600", buckets[0].Txs, buckets[1].Txs)
	}
	if got := sustainedTPS(buckets); got != 800 {
		t.Errorf("sustained = %.0f, want 800 (1600 txs over 2 active seconds)", got)
	}
	// Head is what the report attributes the peak second to.
	if buckets[0].Head != 6 {
		t.Errorf("bucket head = %d, want the last block of that second (6)", buckets[0].Head)
	}
}

// TestBucketTPSKeepsInteriorStalls guards against flattering a chain that
// stalls mid-run: a second with blocks but no transactions inside the active
// window counts as a zero, it is not skipped.
func TestBucketTPSKeepsInteriorStalls(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	var points []blockPoint
	points = append(points, blocksAt(1, base, 2, 900)...)
	points = append(points, blocksAt(3, base.Add(time.Second), 2, 0)...) // stall
	points = append(points, blocksAt(5, base.Add(2*time.Second), 2, 900)...)

	buckets := bucketTPS(points)
	if len(buckets) != 3 {
		t.Fatalf("active seconds = %d, want 3 (the stalled second counts)", len(buckets))
	}
	// Seconds carry 1800, 0, 1800 (two blocks each).
	if buckets[1].Txs != 0 {
		t.Errorf("stalled second = %d tx/s, want 0", buckets[1].Txs)
	}
	if got := sustainedTPS(buckets); got != 1200 {
		t.Errorf("sustained = %.0f, want 1200 (3600 txs over 3 seconds, stall included)", got)
	}
	if got := medianTPS(buckets); got != 1800 {
		t.Errorf("median of {0, 1800, 1800} = %.0f, want 1800", got)
	}
}

func TestBucketTPSEmpty(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	if got := bucketTPS(blocksAt(1, base, 3, 0)); got != nil {
		t.Errorf("a run that included nothing has no active window, got %v", got)
	}
	if got := sustainedTPS(nil); got != 0 {
		t.Errorf("sustained on no buckets = %v, want 0", got)
	}
}

// TestRunStatsTooShort pins the guard for the case that prompted it: a 20,000
// tx load drains in ~2 seconds on a fast chain, so top_tps has 2 entries (one
// per active second) and no second ever had a backlog. The rate is then set by
// the load, not the chain, and must be flagged rather than quoted.
func TestRunStatsTooShort(t *testing.T) {
	for _, tc := range []struct {
		activeSeconds int
		want          bool
	}{
		{0, false}, // nothing included at all: a different problem
		{2, true},
		{4, true},
		{5, false},
		{18, false},
	} {
		got := RunStats{ActiveSeconds: tc.activeSeconds}.TooShort()
		if got != tc.want {
			t.Errorf("ActiveSeconds=%d TooShort()=%v, want %v", tc.activeSeconds, got, tc.want)
		}
	}
}
