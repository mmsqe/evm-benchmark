package bench

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

func WaitForPort(ctx context.Context, host string, port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("%s:%d", host, port)
	for {
		d := net.Dialer{Timeout: time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout waiting for %s: %w", addr, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func WaitForHeight(ctx context.Context, client *http.Client, rpcURL string, target int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		h, err := CurrentHeight(ctx, client, rpcURL)
		if err == nil && h >= target {
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("timeout waiting for height %d: %w", target, err)
			}
			return fmt.Errorf("timeout waiting for height %d, latest %d", target, h)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func DetectIdleOrHalt(ctx context.Context, client *http.Client, rpcURL string, idleBlocks int, pollInterval, haltAfter time.Duration) error {
	lastHeight := int64(0)
	lastProgressAt := time.Now()

	for {
		h, err := CurrentHeight(ctx, client, rpcURL)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}

		if h > lastHeight {
			lastHeight = h
			lastProgressAt = time.Now()

			idle := true
			checked := 0
			for i := int64(0); i < int64(idleBlocks); i++ {
				target := h - i
				if target <= 0 {
					idle = false
					break
				}
				checked++
				blk, err := BlockByNumber(ctx, client, rpcURL, target)
				if err != nil {
					idle = false
					break
				}
				if len(blk.Transactions) > 0 {
					idle = false
					break
				}
			}
			if idle {
				pending, err := TxPoolPendingCount(ctx, client, rpcURL)
				if err != nil {
					fmt.Printf("[bench] idle confirmed at height=%d (txpool status failed: %v)\n", h, err)
				} else {
					fmt.Printf("[bench] idle confirmed at height=%d pending_txpool=%d\n", h, pending)
				}
				return nil
			}

			if checked < idleBlocks {
				fmt.Printf("[bench] idle check deferred at height=%d: need %d blocks, only %d available\n", h, idleBlocks, checked)
			}
		} else if time.Since(lastProgressAt) >= haltAfter {
			fmt.Printf("[bench] halt detected: no block progress for %s at height=%d\n", haltAfter, h)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// WaitForTxs is DetectIdleOrHalt for a shared chain, whose blocks are never
// empty. Reading blocks from `from` on, it returns once every transaction in
// own is included, once idleBlocks blocks pass without any of them (counted
// from the call or the last inclusion), or once the chain stalls for haltAfter.
func WaitForTxs(ctx context.Context, client *http.Client, rpcURL string, own TxSet, from int64, idleBlocks int, pollInterval, haltAfter time.Duration) error {
	remaining := len(own)
	next := from
	quietSince := int64(-1) // head at the first poll, then the last block carrying ours
	lastHead := int64(-1)
	lastProgressAt := time.Now()

	for {
		h, err := CurrentHeight(ctx, client, rpcURL)
		if err == nil {
			if quietSince < 0 {
				quietSince = h
			}
			if h > lastHead {
				lastHead = h
				lastProgressAt = time.Now()
			}
			for ; next <= h; next++ {
				blk, err := BlockByNumber(ctx, client, rpcURL, next)
				if err != nil {
					break // resumed from this height on the next poll
				}
				if n := own.Count(blk.Transactions); n > 0 {
					remaining -= n
					quietSince = max(quietSince, next)
				}
			}
			if remaining <= 0 {
				fmt.Printf("[bench] all %d transactions included by height=%d\n", len(own), next-1)
				return nil
			}
			if quiet := next - 1 - quietSince; quiet >= int64(idleBlocks) {
				fmt.Printf("[bench] %d of %d transactions still missing after %d blocks without any: treating them as dropped\n",
					remaining, len(own), quiet)
				return nil
			}
		}
		if time.Since(lastProgressAt) >= haltAfter {
			fmt.Printf("[bench] halt detected: no block progress for %s at height=%d\n", haltAfter, lastHead)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
