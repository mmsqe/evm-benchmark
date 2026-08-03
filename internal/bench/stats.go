package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type blockPoint struct {
	Height int64
	Txs    int
	At     time.Time
	TPS    float64
}

// tpsBucket is one wall-clock second of block production.
type tpsBucket struct {
	At   time.Time
	Head int64 // highest block height stamped with this second
	Txs  int
}

// bucketTPS groups blocks into whole-second buckets by block timestamp.
//
// Block timestamps are second-granularity on every chain here, so a bucket is
// exactly one second wide: a bucket's transaction count IS that second's rate,
// with no division by a quantized interval. A sliding window over block
// timestamps cannot do that. It divides by the difference of two timestamps,
// which is only meaningful when the window spans several seconds — true at
// ~2.5 blocks/s (tempo: 5 blocks ≈ 2s), false at ~15 blocks/s (allegro: 5
// blocks ≈ 0.3s), where the two timestamps are usually equal (rate reported as
// 0) and otherwise a whole second apart (rate understated ~3x). That made a
// fast chain look ~3x slower than it was.
//
// Buckets cover the active window only — the first through last second that
// carried transactions — so the empty blocks before the load and the idle tail
// after it do not dilute the rate. Idle seconds *inside* that window are kept:
// they are real stalls.
func bucketTPS(points []blockPoint) []tpsBucket {
	first, last := -1, -1
	for i, p := range points {
		if p.Txs > 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}

	buckets := make([]tpsBucket, 0, 16)
	index := make(map[int64]int, 16)
	for _, p := range points[first : last+1] {
		key := p.At.Unix()
		i, ok := index[key]
		if !ok {
			buckets = append(buckets, tpsBucket{At: p.At})
			i = len(buckets) - 1
			index[key] = i
		}
		buckets[i].Txs += p.Txs
		if p.Height > buckets[i].Head {
			buckets[i].Head = p.Height
		}
	}
	return buckets
}

// meanTPS averages a run of whole-second buckets.
func meanTPS(buckets []tpsBucket) float64 {
	if len(buckets) == 0 {
		return 0
	}
	total := 0
	for _, b := range buckets {
		total += b.Txs
	}
	return float64(total) / float64(len(buckets))
}

// sustainedTPS covers the whole active window, partial edges included, so it
// moves with where the load fell against a second boundary. Prefer
// fullSecondTPS.
func sustainedTPS(buckets []tpsBucket) float64 { return meanTPS(buckets) }

// fullSecondTPS is the rate over seconds that lie entirely inside the load: the
// first and last active seconds are partial by construction (the load starts
// and ends mid-second), so they drag the average down by however much of those
// two seconds carried no load. That is pure alignment noise — two runs of the
// same binary can differ 14% on sustained while agreeing to 0.2% here — so this
// is the figure to compare across runs. Returns 0 below three active seconds,
// where nothing is interior.
func fullSecondTPS(buckets []tpsBucket) float64 { return meanTPS(interiorBuckets(buckets)) }

// interiorBuckets drops the partial first and last seconds.
func interiorBuckets(buckets []tpsBucket) []tpsBucket {
	if len(buckets) < 3 {
		return nil
	}
	return buckets[1 : len(buckets)-1]
}

// medianTPS is the middle second of the active window — a better summary than
// the peak, which plan.md notes falls off steeply.
func medianTPS(buckets []tpsBucket) float64 {
	if len(buckets) == 0 {
		return 0
	}
	rates := make([]int, 0, len(buckets))
	for _, b := range buckets {
		rates = append(rates, b.Txs)
	}
	sort.Ints(rates)
	return float64(rates[len(rates)/2])
}

func parseBlockTimestamp(raw string) (int64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty timestamp")
	}

	if ts, err := strconv.ParseInt(trimmed, 0, 64); err == nil {
		return ts, nil
	}

	if ts, err := strconv.ParseInt(trimmed, 16, 64); err == nil {
		return ts, nil
	}

	return 0, fmt.Errorf("invalid timestamp format: %q", raw)
}

// RunStats is what a run measured. Rates are per whole second (see bucketTPS);
// ActiveSeconds is how many seconds of block production carried the load, and
// is the run's resolution: a handful of seconds cannot show a chain's ceiling
// because the load drains before any second is saturated.
type RunStats struct {
	Peaks        []blockPoint
	IncludedTxs  int
	SustainedTPS float64
	// FullSecondTPS excludes the partial first and last seconds; compare this
	// across runs, not SustainedTPS.
	FullSecondTPS float64
	MedianTPS     float64
	ActiveSeconds int
	FullSeconds   int
}

// tpsMinFullSeconds is the fewest whole seconds of production worth quoting a
// rate from. Two of a run's active seconds are always partial, so a five-second
// run samples only three whole ones and its numbers move with where the load
// fell relative to the second boundary.
const tpsMinFullSeconds = 5

// TooShort reports whether the run produced too few whole seconds to compare.
func (s RunStats) TooShort() bool {
	return s.ActiveSeconds > 0 && s.FullSeconds < tpsMinFullSeconds
}

func DumpBlockStats(ctx context.Context, out io.Writer, client *http.Client, rpcURL string, startHeight, endHeight int64, txsSent int) (RunStats, error) {
	const blockReadRetries = 8
	const blockReadRetryDelay = 300 * time.Millisecond
	const blockFetchConcurrency = 12
	if endHeight < startHeight {
		return RunStats{}, nil
	}

	type fetchedBlock struct {
		height int64
		point  blockPoint
		err    error
	}

	jobs := make(chan int64, blockFetchConcurrency)
	results := make(chan fetchedBlock, int(endHeight-startHeight+1))

	workerCount := blockFetchConcurrency
	if total := int(endHeight - startHeight + 1); total < workerCount {
		workerCount = total
	}
	if workerCount < 1 {
		workerCount = 1
	}

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				var point blockPoint
				var lastErr error
				ok := false

				for attempt := 1; attempt <= blockReadRetries; attempt++ {
					blk, err := BlockByNumber(ctx, client, rpcURL, h)
					if err != nil {
						lastErr = fmt.Errorf("read block: %w", err)
					} else {
						tsRaw, tsErr := parseBlockTimestamp(blk.Timestamp)
						if tsErr != nil {
							lastErr = fmt.Errorf("parse timestamp %q: %w", blk.Timestamp, tsErr)
						} else {
							point = blockPoint{Height: h, Txs: len(blk.Transactions), At: time.Unix(tsRaw, 0)}
							ok = true
							break
						}
					}

					if attempt < blockReadRetries {
						select {
						case <-ctx.Done():
							results <- fetchedBlock{height: h, err: ctx.Err()}
							goto nextJob
						case <-time.After(blockReadRetryDelay):
						}
					}
				}

				if !ok {
					results <- fetchedBlock{height: h, err: lastErr}
				} else {
					results <- fetchedBlock{height: h, point: point}
				}

			nextJob:
			}
		}()
	}

	for h := startHeight; h <= endHeight; h++ {
		jobs <- h
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	byHeight := make(map[int64]fetchedBlock, endHeight-startHeight+1)
	for r := range results {
		if r.err == ctx.Err() && ctx.Err() != nil {
			return RunStats{}, ctx.Err()
		}
		byHeight[r.height] = r
	}

	points := make([]blockPoint, 0, endHeight-startHeight+1)
	totalIncludedTxs := 0
	nonEmptyBlocks := 0

	for h := startHeight; h <= endHeight; h++ {
		fetched, ok := byHeight[h]
		if !ok || fetched.err != nil {
			if ok {
				_, _ = fmt.Fprintf(out, "height=%d skipped: %v\n", h, fetched.err)
			} else {
				_, _ = fmt.Fprintf(out, "height=%d skipped: missing fetch result\n", h)
			}
			continue
		}
		points = append(points, fetched.point)
		totalIncludedTxs += fetched.point.Txs
		if fetched.point.Txs > 0 {
			nonEmptyBlocks++
		}
	}

	// Rates are per whole second (see bucketTPS); a block's reported rate is
	// the rate of the second it landed in, so the column stays consistent with
	// the summary instead of being a window that may not span a second at all.
	buckets := bucketTPS(points)
	rateBySecond := make(map[int64]float64, len(buckets))
	for _, b := range buckets {
		rateBySecond[b.At.Unix()] = float64(b.Txs)
	}
	for i, p := range points {
		points[i].TPS = rateBySecond[p.At.Unix()]
		if p.Txs > 0 {
			_, _ = fmt.Fprintf(out, "height=%d time=%s txs=%d tps=%.2f\n",
				p.Height, p.At.UTC().Format(time.RFC3339Nano), p.Txs, points[i].TPS)
		}
	}

	missingTxs := txsSent - totalIncludedTxs
	if missingTxs < 0 {
		missingTxs = 0
	}
	_, _ = fmt.Fprintf(out, "tx_summary sent=%d included=%d missing=%d non_empty_blocks=%d\n", txsSent, totalIncludedTxs, missingTxs, nonEmptyBlocks)
	stats := RunStats{
		IncludedTxs:   totalIncludedTxs,
		SustainedTPS:  sustainedTPS(buckets),
		FullSecondTPS: fullSecondTPS(buckets),
		MedianTPS:     medianTPS(buckets),
		ActiveSeconds: len(buckets),
		FullSeconds:   len(interiorBuckets(buckets)),
	}
	_, _ = fmt.Fprintf(out,
		"tps_summary full_second=%.2f sustained=%.2f median_second=%.2f active_seconds=%d full_seconds=%d\n",
		stats.FullSecondTPS, stats.SustainedTPS, stats.MedianTPS, stats.ActiveSeconds, stats.FullSeconds)
	if stats.TooShort() {
		// One entry per active second, so a short run also explains a short
		// top_tps list — and none of its seconds ever had a backlog to chew on.
		_, _ = fmt.Fprintf(out,
			"tps_warning only %d whole second(s) of production (%d active, two of them partial): the rate here "+
				"moves with where the load fell relative to the second boundary, and is bounded by "+
				"num_accounts*num_txs (%d) rather than by the chain. Raise the load until full_seconds >= %d.\n",
			stats.FullSeconds, stats.ActiveSeconds, txsSent, tpsMinFullSeconds)
	}

	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Txs > buckets[j].Txs })
	top := min(len(buckets), 5)
	stats.Peaks = make([]blockPoint, 0, top)
	for i := 0; i < top; i++ {
		b := buckets[i]
		_, _ = fmt.Fprintf(out, "top_tps rank=%d height=%d time=%s txs=%d tps=%.2f\n",
			i+1, b.Head, b.At.UTC().Format(time.RFC3339Nano), b.Txs, float64(b.Txs))
		stats.Peaks = append(stats.Peaks, blockPoint{Height: b.Head, Txs: b.Txs, At: b.At, TPS: float64(b.Txs)})
	}

	return stats, nil
}
