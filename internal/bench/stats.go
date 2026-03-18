package bench

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
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

func DumpBlockStats(ctx context.Context, out io.Writer, client *http.Client, rpcURL string, startHeight, endHeight int64) ([]float64, error) {
	const tpsWindow = 5
	if endHeight < startHeight {
		return nil, nil
	}

	points := make([]blockPoint, 0, endHeight-startHeight+1)
	tpsList := make([]float64, 0, endHeight-startHeight+1)

	for h := startHeight; h <= endHeight; h++ {
		blk, err := BlockByNumber(ctx, client, rpcURL, h)
		if err != nil {
			return nil, fmt.Errorf("read block %d: %w", h, err)
		}

		tsRaw, err := strconv.ParseInt(blk.Timestamp[2:], 16, 64)
		if err != nil {
			return nil, fmt.Errorf("parse block timestamp %q: %w", blk.Timestamp, err)
		}

		point := blockPoint{Txs: len(blk.Transactions), At: time.Unix(tsRaw, 0)}
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
