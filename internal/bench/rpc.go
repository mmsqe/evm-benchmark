package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      int         `json:"id"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *jsonRPCError   `json:"error"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func JSONRPCCall(ctx context.Context, client *http.Client, url, method string, params interface{}, out interface{}) error {
	body, err := json.Marshal(jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1})
	if err != nil {
		return fmt.Errorf("marshal rpc request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create rpc request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var rpcResp jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return fmt.Errorf("decode rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return fmt.Errorf("json-rpc error (%d): %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	if out != nil {
		if err := json.Unmarshal(rpcResp.Result, out); err != nil {
			return fmt.Errorf("decode rpc result: %w", err)
		}
	}

	return nil
}

// DefaultPendingWatermark is how many pending transactions the sender will let
// pile up before it pauses. It exists to keep a slow chain's mempool from
// overflowing, but it also caps how deep a backlog a chain is ever given: a
// chain that drains faster than the sender refills is then measured on the
// sender's loop, not its own execution. Benchmarks comparing chains of
// different speeds should raise it past the whole load so it never binds —
// see broadcast_pending_watermark.
const DefaultPendingWatermark = int64(5000)

// BroadcastStats describes how the send itself went. It answers the question a
// throughput number cannot: was the chain ever behind the sender? If MaxPending
// stays small, the chain drained everything as fast as it arrived and the
// measured rate is bounded by how fast transactions could be submitted, not by
// how fast the chain can execute them — in which case a change to block
// building will not move the number.
type BroadcastStats struct {
	Sent       int
	Duration   time.Duration
	MaxPending int64
}

// Rate is transactions submitted per second.
func (b BroadcastStats) Rate() float64 {
	if b.Duration <= 0 {
		return 0
	}
	return float64(b.Sent) / b.Duration.Seconds()
}

// BroadcastRawTxs sends every transaction, pausing while more than watermark
// are pending (watermark <= 0 uses DefaultPendingWatermark).
func BroadcastRawTxs(ctx context.Context, client *http.Client, rpcURL string, txs []string, concurrency int, watermark int64) BroadcastStats {
	if concurrency < 1 {
		concurrency = 1
	}
	if watermark <= 0 {
		watermark = DefaultPendingWatermark
	}

	const txpoolBackoff = 100 * time.Millisecond
	// Polling the pool before every transaction doubles the RPC cost of a send.
	// When the watermark cannot be reached by this load it can never pause, so
	// sample it periodically for the diagnostic instead of gating on it.
	pollEvery := 1
	if watermark >= int64(len(txs)) {
		pollEvery = 256
	}

	started := time.Now()
	var maxPending atomic.Int64
	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sincePoll := 0
			for raw := range jobs {
				for {
					sincePoll++
					if sincePoll%pollEvery != 0 {
						break
					}
					pending, err := TxPoolPendingCount(ctx, client, rpcURL)
					if err == nil {
						for {
							prev := maxPending.Load()
							if pending <= prev || maxPending.CompareAndSwap(prev, pending) {
								break
							}
						}
					}
					if err == nil && pending > watermark {
						select {
						case <-ctx.Done():
							return
						case <-time.After(txpoolBackoff):
						}
						continue
					}
					break
				}

				var result string
				_ = JSONRPCCall(ctx, client, rpcURL, "eth_sendRawTransaction", []string{raw}, &result)
			}
		}()
	}

	for _, tx := range txs {
		jobs <- tx
	}
	close(jobs)
	wg.Wait()

	return BroadcastStats{Sent: len(txs), Duration: time.Since(started), MaxPending: maxPending.Load()}
}

func CurrentHeight(ctx context.Context, client *http.Client, rpcURL string) (int64, error) {
	var hexHeight string
	if err := JSONRPCCall(ctx, client, rpcURL, "eth_blockNumber", []interface{}{}, &hexHeight); err != nil {
		return 0, err
	}
	var h int64
	if _, err := fmt.Sscanf(hexHeight, "0x%x", &h); err != nil {
		return 0, fmt.Errorf("parse block height %q: %w", hexHeight, err)
	}
	return h, nil
}

func SuggestedGasPrice(ctx context.Context, client *http.Client, rpcURL string) (int64, error) {
	var hexPrice string
	if err := JSONRPCCall(ctx, client, rpcURL, "eth_gasPrice", []interface{}{}, &hexPrice); err != nil {
		return 0, err
	}

	p, err := strconv.ParseInt(hexPrice, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parse gas price %q: %w", hexPrice, err)
	}

	return p, nil
}

type ethBlock struct {
	Timestamp    string            `json:"timestamp"`
	Transactions []json.RawMessage `json:"transactions"`
}

func BlockByNumber(ctx context.Context, client *http.Client, rpcURL string, height int64) (ethBlock, error) {
	var blk ethBlock
	if err := JSONRPCCall(ctx, client, rpcURL, "eth_getBlockByNumber", []interface{}{fmt.Sprintf("0x%x", height), false}, &blk); err != nil {
		return ethBlock{}, err
	}
	return blk, nil
}

type txPoolStatus struct {
	Pending string `json:"pending"`
	Queued  string `json:"queued"`
}

func TxPoolPendingCount(ctx context.Context, client *http.Client, rpcURL string) (int64, error) {
	var status txPoolStatus
	if err := JSONRPCCall(ctx, client, rpcURL, "txpool_status", []interface{}{}, &status); err != nil {
		return 0, err
	}

	pending, err := strconv.ParseInt(status.Pending, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parse txpool pending %q: %w", status.Pending, err)
	}
	queued, err := strconv.ParseInt(status.Queued, 0, 64)
	if err != nil {
		return 0, fmt.Errorf("parse txpool queued %q: %w", status.Queued, err)
	}

	return pending + queued, nil
}
