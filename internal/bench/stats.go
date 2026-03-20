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
)

type blockPoint struct {
	Txs int
	At  time.Time
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

func DumpBlockStats(ctx context.Context, out io.Writer, client *http.Client, rpcURL string, startHeight, endHeight int64) ([]float64, error) {
	const tpsWindow = 5
	const blockReadRetries = 8
	const blockReadRetryDelay = 300 * time.Millisecond
	if endHeight < startHeight {
		return nil, nil
	}

	points := make([]blockPoint, 0, endHeight-startHeight+1)
	tpsList := make([]float64, 0, endHeight-startHeight+1)

	for h := startHeight; h <= endHeight; h++ {
		var point blockPoint
		ok := false
		var lastErr error

		for attempt := 1; attempt <= blockReadRetries; attempt++ {
			blk, err := BlockByNumber(ctx, client, rpcURL, h)
			if err != nil {
				lastErr = fmt.Errorf("read block: %w", err)
			} else {
				tsRaw, tsErr := parseBlockTimestamp(blk.Timestamp)
				if tsErr != nil {
					lastErr = fmt.Errorf("parse timestamp %q: %w", blk.Timestamp, tsErr)
				} else {
					point = blockPoint{Txs: len(blk.Transactions), At: time.Unix(tsRaw, 0)}
					ok = true
					break
				}
			}

			if attempt < blockReadRetries {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(blockReadRetryDelay):
				}
			}
		}

		if !ok {
			_, _ = fmt.Fprintf(out, "height=%d skipped: %v\n", h, lastErr)
			continue
		}

		points = append(points, point)

		windowStart := len(points) - tpsWindow
		if windowStart < 0 {
			windowStart = 0
		}
		tp := calculateTPS(points[windowStart:])
		tpsList = append(tpsList, tp)

		_, _ = fmt.Fprintf(out, "height=%d time=%s txs=%d tps=%.2f\n", h, point.At.UTC().Format(time.RFC3339Nano), point.Txs, tp)
	}

	sort.Slice(tpsList, func(i, j int) bool { return tpsList[i] > tpsList[j] })
	top := 5
	if len(tpsList) < top {
		top = len(tpsList)
	}
	_, _ = fmt.Fprintf(out, "top 5 TPS: %v\n", tpsList[:top])

	return tpsList[:top], nil
}
