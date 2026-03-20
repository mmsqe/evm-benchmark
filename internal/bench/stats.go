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

func calculateTPS(window []blockPoint) float64 {
	if len(window) < 2 {
		return 0
	}
	txCount := 0
	for i := 1; i < len(window); i++ {
		txCount += window[i].Txs
	}
	seconds := window[len(window)-1].At.Sub(window[0].At).Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(txCount) / seconds
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

func DumpBlockStats(ctx context.Context, out io.Writer, client *http.Client, rpcURL string, startHeight, endHeight int64, txsSent int) ([]blockPoint, int, error) {
	const tpsWindow = 5
	const blockReadRetries = 8
	const blockReadRetryDelay = 300 * time.Millisecond
	const blockFetchConcurrency = 12
	if endHeight < startHeight {
		return nil, 0, nil
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
			return nil, 0, ctx.Err()
		}
		byHeight[r.height] = r
	}

	points := make([]blockPoint, 0, endHeight-startHeight+1)
	tpsList := make([]float64, 0, endHeight-startHeight+1)
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
		point := fetched.point

		points = append(points, point)

		windowStart := len(points) - tpsWindow
		if windowStart < 0 {
			windowStart = 0
		}
		tp := calculateTPS(points[windowStart:])
		points[len(points)-1].TPS = tp
		tpsList = append(tpsList, tp)
		totalIncludedTxs += point.Txs
		if point.Txs > 0 {
			nonEmptyBlocks++
		}

		if point.Txs > 0 {
			_, _ = fmt.Fprintf(out, "height=%d time=%s txs=%d tps=%.2f\n", h, point.At.UTC().Format(time.RFC3339Nano), point.Txs, tp)
		}
	}

	missingTxs := txsSent - totalIncludedTxs
	if missingTxs < 0 {
		missingTxs = 0
	}
	_, _ = fmt.Fprintf(out, "tx_summary sent=%d included=%d missing=%d non_empty_blocks=%d\n", txsSent, totalIncludedTxs, missingTxs, nonEmptyBlocks)

	sort.Slice(tpsList, func(i, j int) bool { return tpsList[i] > tpsList[j] })
	sort.Slice(points, func(i, j int) bool { return points[i].TPS > points[j].TPS })
	top := 5
	if len(tpsList) < top {
		top = len(tpsList)
	}
	for i := 0; i < top; i++ {
		entry := points[i]
		_, _ = fmt.Fprintf(out, "top_tps rank=%d height=%d time=%s txs=%d tps=%.2f\n", i+1, entry.Height, entry.At.UTC().Format(time.RFC3339Nano), entry.Txs, entry.TPS)
	}

	return points[:top], totalIncludedTxs, nil
}
