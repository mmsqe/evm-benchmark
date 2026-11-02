package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
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
	Head int64 // highest block height stamped with this second, 0 if none was
	Txs  int
	Rate float64 // Txs over the seconds until the next block, see bucketTPS
}

// bucketTPS groups blocks into whole-second buckets by block timestamp, which
// has second resolution, so a bucket's transaction count is that second's rate
// (see "Measuring" in the README).
//
// Buckets cover the first through last second that carried transactions, so the
// empty blocks around the load do not dilute the rate. Every second inside is
// kept, with or without a block: an idle second there is a real stall.
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

	// Block timestamps never decrease, so the first and last loaded blocks bound
	// the window.
	start := points[first].At.Unix()
	buckets := make([]tpsBucket, points[last].At.Unix()-start+1)
	for i := range buckets {
		buckets[i].At = time.Unix(start+int64(i), 0)
	}
	for _, p := range points[first : last+1] {
		b := &buckets[p.At.Unix()-start]
		b.Txs += p.Txs
		b.Head = max(b.Head, p.Height)
	}

	// A block is built until the next one, so its second's rate spreads the
	// transactions over the seconds until the next block. At a block per second
	// or faster, that is the second's own count.
	end := points[last].At.Unix()
	next := end + 1
	for _, p := range points[last+1:] {
		if s := p.At.Unix(); s > end {
			next = s
			break
		}
	}
	for i := len(buckets) - 1; i >= 0; i-- {
		if b := &buckets[i]; b.Head > 0 {
			b.Rate = float64(b.Txs) / float64(next-b.At.Unix())
			next = b.At.Unix()
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

// fullSecondTPS is [RunStats.FullSecondTPS]; 0 below three active seconds, where
// nothing is interior.
func fullSecondTPS(buckets []tpsBucket) float64 { return meanTPS(interiorBuckets(buckets)) }

// interiorBuckets drops the partial first and last seconds.
func interiorBuckets(buckets []tpsBucket) []tpsBucket {
	if len(buckets) < 3 {
		return nil
	}
	return buckets[1 : len(buckets)-1]
}

// medianTPS is the middle second of the active window — a better summary than
// the peak, which falls off steeply after the first seconds.
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

// drainBuckets keeps the whole seconds after sendEnd, see [RunStats.DrainTPS].
// The last second is partial, as in interiorBuckets, so it is left out.
func drainBuckets(buckets []tpsBucket, sendEnd time.Time) []tpsBucket {
	whole := buckets[:max(len(buckets)-1, 0)]
	return whole[sort.Search(len(whole), func(i int) bool { return whole[i].At.After(sendEnd) }):]
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

// RunStats is what a run measured. Rates are per whole second (see bucketTPS).
type RunStats struct {
	Peaks       []blockPoint
	IncludedTxs int
	// SustainedTPS covers every active second, so it moves with where the load
	// fell against a second boundary.
	SustainedTPS float64
	// FullSecondTPS excludes the partial first and last seconds; compare this
	// across runs.
	FullSecondTPS float64
	MedianTPS     float64
	// ActiveSeconds is how many seconds of block production carried the load.
	ActiveSeconds int
	FullSeconds   int
	// DrainTPS is the rate over the DrainSeconds whole seconds after the sender
	// finished, when block production no longer shares the node with
	// transaction intake. Zero seconds means the chain kept pace with the sender.
	DrainTPS     float64
	DrainSeconds int
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

// DumpBlockStats reads blocks startHeight..endHeight and writes the run's
// summary to out. A non-nil own counts only those transactions: on a shared
// chain the blocks also carry everyone else's. sendEnd is when the sender
// finished, see [RunStats.DrainTPS].
func DumpBlockStats(ctx context.Context, out io.Writer, client *http.Client, rpcURL string, startHeight, endHeight int64, txsSent int, sendEnd time.Time, own TxSet) (RunStats, error) {
	if endHeight < startHeight {
		return RunStats{}, nil
	}

	type fetched struct {
		point blockPoint
		err   error
	}
	results := make([]fetched, endHeight-startHeight+1)
	var fetches errgroup.Group
	fetches.SetLimit(12)
	for h := startHeight; h <= endHeight; h++ {
		fetches.Go(func() error {
			point, err := fetchBlockPoint(ctx, client, rpcURL, h, own)
			results[h-startHeight] = fetched{point, err}
			return nil // a block that stays unreadable is reported, not fatal
		})
	}
	_ = fetches.Wait()
	if err := ctx.Err(); err != nil {
		return RunStats{}, err
	}

	points := make([]blockPoint, 0, len(results))
	totalIncludedTxs := 0
	nonEmptyBlocks := 0
	for i, r := range results {
		if r.err != nil {
			_, _ = fmt.Fprintf(out, "height=%d skipped: %v\n", startHeight+int64(i), r.err)
			continue
		}
		points = append(points, r.point)
		totalIncludedTxs += r.point.Txs
		if r.point.Txs > 0 {
			nonEmptyBlocks++
		}
	}

	// A block's reported rate is that of the second it landed in (see
	// bucketTPS), so the column stays consistent with the peaks below.
	buckets := bucketTPS(points)
	rateBySecond := make(map[int64]float64, len(buckets))
	for _, b := range buckets {
		rateBySecond[b.At.Unix()] = b.Rate
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
	drain := drainBuckets(buckets, sendEnd)
	stats := RunStats{
		IncludedTxs:   totalIncludedTxs,
		SustainedTPS:  meanTPS(buckets),
		FullSecondTPS: fullSecondTPS(buckets),
		MedianTPS:     medianTPS(buckets),
		ActiveSeconds: len(buckets),
		FullSeconds:   len(interiorBuckets(buckets)),
		DrainTPS:      meanTPS(drain),
		DrainSeconds:  len(drain),
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

	_, _ = fmt.Fprintf(out, "drain_summary tps=%.2f seconds=%d\n", stats.DrainTPS, stats.DrainSeconds)

	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Rate > buckets[j].Rate })
	top := min(len(buckets), 5)
	stats.Peaks = make([]blockPoint, 0, top)
	for i := 0; i < top; i++ {
		b := buckets[i]
		if b.Rate == 0 {
			break // a second without transactions is no peak
		}
		_, _ = fmt.Fprintf(out, "top_tps rank=%d height=%d time=%s txs=%d tps=%.2f\n",
			i+1, b.Head, b.At.UTC().Format(time.RFC3339Nano), b.Txs, b.Rate)
		stats.Peaks = append(stats.Peaks, blockPoint{Height: b.Head, Txs: b.Txs, At: b.At, TPS: b.Rate})
	}

	return stats, nil
}

// fetchBlockPoint reads one block, retrying a failed read or an unreadable
// timestamp a few times.
func fetchBlockPoint(ctx context.Context, client *http.Client, rpcURL string, height int64, own TxSet) (blockPoint, error) {
	const retries = 8
	const retryDelay = 300 * time.Millisecond
	var err error
	for attempt := 1; ; attempt++ {
		blk, readErr := BlockByNumber(ctx, client, rpcURL, height)
		if readErr != nil {
			err = fmt.Errorf("read block: %w", readErr)
		} else if ts, tsErr := parseBlockTimestamp(blk.Timestamp); tsErr != nil {
			err = fmt.Errorf("parse timestamp %q: %w", blk.Timestamp, tsErr)
		} else {
			txs := len(blk.Transactions)
			if own != nil {
				txs = own.Count(blk.Transactions)
			}
			return blockPoint{Height: height, Txs: txs, At: time.Unix(ts, 0)}, nil
		}
		if attempt == retries {
			return blockPoint{}, err
		}
		select {
		case <-ctx.Done():
			return blockPoint{}, ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}
