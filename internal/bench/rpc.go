package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
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

// sendRawTxBatch submits every transaction in one JSON-RPC batch request and
// returns how many the node rejected. One HTTP round trip per transaction caps
// the sender near 50k tx/s on loopback, which is below what a fast chain can
// execute — at that point the benchmark measures submission, not the chain.
func sendRawTxBatch(ctx context.Context, client *http.Client, url string, raws []string) (rejected int, err error) {
	batch := make([]jsonRPCRequest, 0, len(raws))
	for i, raw := range raws {
		batch = append(batch, jsonRPCRequest{
			JSONRPC: "2.0", Method: "eth_sendRawTransaction", Params: []string{raw}, ID: i + 1,
		})
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return 0, fmt.Errorf("marshal batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// A server that does not support batching answers with a single object;
	// treat that as a hard error rather than silently losing the batch.
	var responses []jsonRPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&responses); err != nil {
		return 0, fmt.Errorf("decode batch response (does this node support JSON-RPC batches?): %w", err)
	}
	for _, r := range responses {
		if r.Error != nil {
			rejected++
		}
	}
	return rejected, nil
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
	// Rejected counts transactions the node refused at submission. Single-send
	// mode cannot see this (the response is discarded); batch mode reports it,
	// which turns a silent "included < sent" into a diagnosable one.
	Rejected int
}

// SaturationFloor is the fraction of the load that must queue up at some point
// for the measured rate to describe the chain. A chain that never accumulates a
// backlog was keeping pace with the sender, so what was measured is how fast
// transactions could be submitted. A saturated chain is the opposite: the sender
// outruns it and the pool grows toward the whole load.
//
// A quarter is well clear of both cases seen in practice — evmd at a 200k load
// backed up to 38% and its rate rose accordingly, while runs that tracked the
// sender sat at 2-6%.
const SaturationFloor = 0.25

// Undersaturated reports whether the chain kept pace with the sender, in which
// case the rate is a lower bound on the chain rather than a measurement of it.
func (b BroadcastStats) Undersaturated() bool {
	return b.Sent > 0 && float64(b.MaxPending) < SaturationFloor*float64(b.Sent)
}

// Rate is transactions submitted per second.
func (b BroadcastStats) Rate() float64 {
	if b.Duration <= 0 {
		return 0
	}
	return float64(b.Sent) / b.Duration.Seconds()
}

// awaitPoolRoom samples the pool, recording its depth, and waits while it is
// above the watermark. Reports false if the context ended.
func awaitPoolRoom(ctx context.Context, client *http.Client, rpcURL string, watermark int64, deepest *atomic.Int64) bool {
	const backoff = 100 * time.Millisecond
	for {
		pending, err := TxPoolPendingCount(ctx, client, rpcURL)
		if err != nil {
			return true // a pool we cannot read cannot throttle us
		}
		for {
			prev := deepest.Load()
			if pending <= prev || deepest.CompareAndSwap(prev, pending) {
				break
			}
		}
		if pending <= watermark {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
	}
}

// BroadcastRawTxs sends every transaction, pausing while more than watermark
// are pending. Negative never pauses; 0 uses DefaultPendingWatermark.
func BroadcastRawTxs(ctx context.Context, client *http.Client, rpcURL string, txs []string, concurrency int, watermark int64, batchSize int) BroadcastStats {
	if concurrency < 1 {
		concurrency = 1
	}
	switch {
	case watermark < 0:
		watermark = math.MaxInt64 // never pause
	case watermark == 0:
		watermark = DefaultPendingWatermark
	}
	if batchSize < 1 {
		batchSize = 1
	}

	// Polling the pool before every transaction doubles the RPC cost of a send.
	// When the watermark cannot be reached by this load it can never pause, so
	// sample it periodically for the diagnostic instead of gating on it.
	// Counted in transactions, not requests, so batching does not silently turn
	// the sample rate down by a factor of batchSize.
	pollEvery := 1
	if watermark >= int64(len(txs)) {
		pollEvery = 256
	}

	started := time.Now()
	var maxPending, rejected atomic.Int64
	jobs := make(chan []string)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sincePoll := 0
			for chunk := range jobs {
				sincePoll += len(chunk)
				if sincePoll >= pollEvery {
					sincePoll = 0
					if !awaitPoolRoom(ctx, client, rpcURL, watermark, &maxPending) {
						return
					}
				}

				// batchSize 1 keeps the plain single-request path, so a node
				// that does not implement JSON-RPC batches still works.
				if len(chunk) == 1 {
					var result string
					_ = JSONRPCCall(ctx, client, rpcURL, "eth_sendRawTransaction", chunk, &result)
					continue
				}
				if n, err := sendRawTxBatch(ctx, client, rpcURL, chunk); err == nil {
					rejected.Add(int64(n))
				}
			}
		}()
	}

	for start := 0; start < len(txs); start += batchSize {
		jobs <- txs[start:min(start+batchSize, len(txs))]
	}
	close(jobs)
	wg.Wait()

	return BroadcastStats{
		Sent:       len(txs),
		Duration:   time.Since(started),
		MaxPending: maxPending.Load(),
		Rejected:   int(rejected.Load()),
	}
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
