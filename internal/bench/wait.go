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
			for i := int64(0); i < int64(idleBlocks); i++ {
				target := h - i
				if target <= 0 {
					break
				}
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
				return nil
			}
		} else if time.Since(lastProgressAt) >= haltAfter {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}
