package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

// rateLimitRetries bounds how long a call waits out HTTP 429: about a minute
// at the usual one-second Retry-After.
const rateLimitRetries = 60

// A request that gets no answer — a timeout, a reset, a 5xx from the proxy —
// is retried this many times, retryBackoff apart. Dropping it would leave a
// gap in an account's nonces, and everything after the gap would queue.
const failedRetries = 3

var retryBackoff = time.Second

// postJSON posts req and decodes the reply into out, retrying what the server
// did not answer and waiting out HTTP 429 as long as it asks.
func postJSON(ctx context.Context, client *http.Client, url string, req, out interface{}) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal rpc request: %w", err)
	}
	wait := func(d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
	for attempt, failures := 0, 0; ; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create rpc request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(httpReq)
		if err == nil && resp.StatusCode == http.StatusTooManyRequests && attempt < rateLimitRetries {
			resp.Body.Close()
			if err := wait(retryAfter(resp.Header.Get("Retry-After"))); err != nil {
				return err
			}
			continue
		}
		if err == nil && resp.StatusCode/100 == 5 {
			err = fmt.Errorf("rpc: HTTP %s", resp.Status)
			resp.Body.Close()
		}
		if err != nil {
			if failures++; failures <= failedRetries {
				if err := wait(retryBackoff); err != nil {
					return err
				}
				continue
			}
			return err
		}
		err = json.NewDecoder(resp.Body).Decode(out)
		resp.Body.Close()
		if err != nil && resp.StatusCode/100 != 2 {
			// Says more than the decoder does: a public endpoint refuses an
			// oversized batch with 413.
			return fmt.Errorf("rpc: HTTP %s", resp.Status)
		}
		if err != nil {
			return fmt.Errorf("decode rpc response: %w", err)
		}
		return nil
	}
}

// retryAfter reads a Retry-After given in seconds, defaulting to one. The
// jitter keeps senders refused together from all retrying at once.
func retryAfter(header string) time.Duration {
	wait := time.Second
	if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
		wait = time.Duration(min(secs, 30)) * time.Second
	}
	return wait + rand.N(250*time.Millisecond)
}

// sendRawTxBatch submits every transaction in one JSON-RPC batch request and
// returns the node's message for each one it rejected. One HTTP round trip per
// transaction caps the sender near 50k tx/s on loopback, which is below what a
// fast chain can execute — at that point the benchmark measures submission,
// not the chain.
func sendRawTxBatch(ctx context.Context, client *http.Client, url string, raws []string) (rejected []string, err error) {
	batch := make([]jsonRPCRequest, 0, len(raws))
	for i, raw := range raws {
		batch = append(batch, jsonRPCRequest{
			JSONRPC: "2.0", Method: "eth_sendRawTransaction", Params: []string{raw}, ID: i + 1,
		})
	}

	// A server that does not support batching answers with a single object;
	// treat that as a hard error rather than silently losing the batch.
	var responses []jsonRPCResponse
	if err := postJSON(ctx, client, url, batch, &responses); err != nil {
		return nil, fmt.Errorf("send batch (does this node support JSON-RPC batches?): %w", err)
	}
	for _, r := range responses {
		// "already known" answers a retry of a request the node did admit
		// before losing the reply: the transaction is in, not refused.
		if r.Error != nil && r.Error.Message != "already known" {
			rejected = append(rejected, r.Error.Message)
		}
	}
	return rejected, nil
}

// numbers masks the nonces, hashes and amounts in a rejection message, so one
// cause counts as one reason.
var numbers = regexp.MustCompile(`0x[0-9a-fA-F]+|\d+`)

// RPCCall is one call of a JSONRPCBatch.
type RPCCall struct {
	Method string
	Params interface{}
}

// JSONRPCBatch runs calls in batches of at most maxPerRequest and returns
// their results in call order. One failed call fails them all: it is for reads
// that must all succeed.
func JSONRPCBatch(ctx context.Context, client *http.Client, url string, calls []RPCCall, maxPerRequest int) ([]json.RawMessage, error) {
	maxPerRequest = max(maxPerRequest, 1)
	results := make([]json.RawMessage, len(calls))
	for start := 0; start < len(calls); start += maxPerRequest {
		end := min(start+maxPerRequest, len(calls))
		batch := make([]jsonRPCRequest, 0, end-start)
		for i := start; i < end; i++ {
			// The id is the result's index: answers may come back in any order.
			batch = append(batch, jsonRPCRequest{JSONRPC: "2.0", Method: calls[i].Method, Params: calls[i].Params, ID: i})
		}
		var responses []jsonRPCResponse
		if err := postJSON(ctx, client, url, batch, &responses); err != nil {
			return nil, err
		}
		for _, r := range responses {
			switch {
			case r.ID < start || r.ID >= end:
				return nil, fmt.Errorf("batch answered unknown id %d", r.ID)
			case r.Error != nil:
				return nil, fmt.Errorf("%s: json-rpc error (%d): %s", calls[r.ID].Method, r.Error.Code, r.Error.Message)
			}
			results[r.ID] = r.Result
		}
	}
	for i, r := range results {
		if r == nil {
			return nil, fmt.Errorf("batch never answered %s (call %d)", calls[i].Method, i)
		}
	}
	return results, nil
}

func JSONRPCCall(ctx context.Context, client *http.Client, url, method string, params interface{}, out interface{}) error {
	var rpcResp jsonRPCResponse
	if err := postJSON(ctx, client, url, jsonRPCRequest{JSONRPC: "2.0", Method: method, Params: params, ID: 1}, &rpcResp); err != nil {
		return err
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
	// Rejected counts transactions the node refused at submission, and
	// RejectReasons groups them by its message. Single-send mode cannot see
	// this (the response is discarded); batch mode reports it, which turns a
	// silent "included < sent" into a diagnosable one.
	Rejected      int
	RejectReasons map[string]int
	// PoolUnknown means the pool was never read — the endpoint does not serve
	// txpool_status, or the load ended before the first sample — so MaxPending
	// is not a measurement.
	PoolUnknown bool
	// PoolStalled means the pool stopped draining above the watermark, so the
	// rest of the load was sent without waiting (see poolStallTimeout).
	PoolStalled bool
	// Failed counts transactions in requests the node never answered, retries
	// included; whether it admitted them shows up in the blocks.
	Failed int
	// Request latency: how long the node took to answer a send. Growing with
	// the number of workers while the rate stays flat means the node answers
	// one request at a time.
	ReqP50, ReqP95, ReqMax time.Duration
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
// A pool that was never read says nothing either way.
func (b BroadcastStats) Undersaturated() bool {
	return !b.PoolUnknown && b.Sent > 0 && float64(b.MaxPending) < SaturationFloor*float64(b.Sent)
}

// Rate is transactions submitted per second.
func (b BroadcastStats) Rate() float64 {
	if b.Duration <= 0 {
		return 0
	}
	return float64(b.Sent) / b.Duration.Seconds()
}

// poolStallTimeout bounds a wait on a pool that has stopped draining.
var poolStallTimeout = 2 * time.Minute

// awaitPoolRoom samples the pool, recording its depth, and waits while more
// than watermark transactions are executable. Reports false if the context
// ended. Neither of the things that cannot drain is waited on: queued
// transactions, which wait on nonces a paused sender may hold, and a pool that
// has not drained for poolStallTimeout, which a node that never forwards some
// transactions leaves behind (stalled is then set, and no sender waits again).
func awaitPoolRoom(ctx context.Context, client *http.Client, rpcURL string, watermark int64, deepest *atomic.Int64, stalled *atomic.Bool) bool {
	const backoff = 100 * time.Millisecond
	lowest, since := int64(math.MaxInt64), time.Now()
	for {
		pending, _, err := TxPoolStatus(ctx, client, rpcURL)
		if err != nil {
			return true // a pool we cannot read cannot throttle us
		}
		for {
			prev := deepest.Load()
			if pending <= prev || deepest.CompareAndSwap(prev, pending) {
				break
			}
		}
		if pending <= watermark || stalled.Load() {
			return true
		}
		if pending < lowest {
			lowest, since = pending, time.Now()
		} else if time.Since(since) >= poolStallTimeout {
			stalled.Store(true)
			fmt.Printf("[bench] pool stuck at %d pending for %s: sending the rest without waiting\n", pending, poolStallTimeout)
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
	}
}

// BroadcastOptions shapes a send.
type BroadcastOptions struct {
	Concurrency int // requests in flight
	BatchSize   int // transactions per request; 1 skips JSON-RPC batching
	// Watermark pauses sending while more transactions are pending. Negative
	// never pauses; 0 uses DefaultPendingWatermark.
	Watermark int64
}

// BroadcastRawTxs sends txs in whatever order free workers take them.
func BroadcastRawTxs(ctx context.Context, client *http.Client, rpcURL string, txs []string, opts BroadcastOptions) BroadcastStats {
	opts = opts.withDefaults()
	shared := chunk(txs, opts.BatchSize)
	queues := make([]chan []string, opts.Concurrency)
	for w := range queues {
		queues[w] = shared
	}
	return broadcast(ctx, client, rpcURL, queues, len(txs), opts)
}

// BroadcastStreams sends each stream in order: a worker owns whole streams and
// sends one request at a time, so nothing in a stream overtakes what came
// before it. A node that is not the block producer may never forward a nonce
// that arrived before its predecessor, so a remote run sends each account as a
// stream.
func BroadcastStreams(ctx context.Context, client *http.Client, rpcURL string, streams [][]string, opts BroadcastOptions) BroadcastStats {
	opts = opts.withDefaults()
	workers := min(opts.Concurrency, len(streams))
	queues := make([]chan []string, workers)
	total := 0
	for w := range queues {
		var mine []string
		for s := w; s < len(streams); s += workers {
			mine = append(mine, streams[s]...)
		}
		total += len(mine)
		queues[w] = chunk(mine, opts.BatchSize)
	}
	return broadcast(ctx, client, rpcURL, queues, total, opts)
}

// chunk queues txs as batches of at most size, closed once all are queued.
func chunk(txs []string, size int) chan []string {
	q := make(chan []string, len(txs)/size+1)
	for start := 0; start < len(txs); start += size {
		q <- txs[start:min(start+size, len(txs))]
	}
	close(q)
	return q
}

func (o BroadcastOptions) withDefaults() BroadcastOptions {
	o.Concurrency = max(o.Concurrency, 1)
	o.BatchSize = max(o.BatchSize, 1)
	switch {
	case o.Watermark < 0:
		o.Watermark = math.MaxInt64 // never pause
	case o.Watermark == 0:
		o.Watermark = DefaultPendingWatermark
	}
	return o
}

// broadcast runs one worker per queue, each sending its queue's batches.
func broadcast(ctx context.Context, client *http.Client, rpcURL string, queues []chan []string, total int, opts BroadcastOptions) BroadcastStats {
	// Polling the pool before every transaction doubles the RPC cost of a send.
	// When the watermark cannot be reached by this load it can never pause, so
	// sample it periodically for the diagnostic instead of gating on it.
	// Counted in transactions, not requests, so batching does not silently turn
	// the sample rate down by a factor of the batch size.
	pollEvery := 1
	if opts.Watermark >= int64(total) {
		pollEvery = 256
	}

	started := time.Now()
	var maxPending atomic.Int64
	maxPending.Store(-1) // until a sample succeeds
	var stalled atomic.Bool
	var (
		mu        sync.Mutex
		reasons   = map[string]int{}
		failed    int
		latencies []time.Duration
	)
	var wg sync.WaitGroup
	for _, queue := range queues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sincePoll := 0
			for chunk := range queue {
				if ctx.Err() != nil {
					return
				}
				sincePoll += len(chunk)
				if sincePoll >= pollEvery {
					sincePoll = 0
					if !awaitPoolRoom(ctx, client, rpcURL, opts.Watermark, &maxPending, &stalled) {
						return
					}
				}

				// batchSize 1 keeps the plain single-request path, so a node
				// that does not implement JSON-RPC batches still works.
				var refused []string
				var err error
				sentAt := time.Now()
				if len(chunk) == 1 {
					var hash string
					err = JSONRPCCall(ctx, client, rpcURL, "eth_sendRawTransaction", chunk, &hash)
				} else {
					refused, err = sendRawTxBatch(ctx, client, rpcURL, chunk)
				}
				mu.Lock()
				latencies = append(latencies, time.Since(sentAt))
				if err != nil {
					failed += len(chunk)
				}
				for _, msg := range refused {
					reasons[numbers.ReplaceAllString(msg, "N")]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	rejected := 0
	for _, n := range reasons {
		rejected += n
	}
	slices.Sort(latencies)
	return BroadcastStats{
		Sent:          total,
		Duration:      time.Since(started),
		MaxPending:    max(maxPending.Load(), 0),
		Rejected:      rejected,
		RejectReasons: reasons,
		PoolUnknown:   maxPending.Load() < 0,
		PoolStalled:   stalled.Load(),
		Failed:        failed,
		ReqP50:        percentile(latencies, 0.5),
		ReqP95:        percentile(latencies, 0.95),
		ReqMax:        percentile(latencies, 1),
	}
}

// percentile of sorted durations; 0 when there are none.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(int(p*float64(len(sorted))), len(sorted)-1)]
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

// TxPoolStatus returns how many pooled transactions are executable (pending)
// and how many wait on a missing lower nonce (queued).
func TxPoolStatus(ctx context.Context, client *http.Client, rpcURL string) (pending, queued int64, err error) {
	var status txPoolStatus
	if err := JSONRPCCall(ctx, client, rpcURL, "txpool_status", []interface{}{}, &status); err != nil {
		return 0, 0, err
	}
	if pending, err = strconv.ParseInt(status.Pending, 0, 64); err != nil {
		return 0, 0, fmt.Errorf("parse txpool pending %q: %w", status.Pending, err)
	}
	if queued, err = strconv.ParseInt(status.Queued, 0, 64); err != nil {
		return 0, 0, fmt.Errorf("parse txpool queued %q: %w", status.Queued, err)
	}
	return pending, queued, nil
}

func TxPoolPendingCount(ctx context.Context, client *http.Client, rpcURL string) (int64, error) {
	pending, queued, err := TxPoolStatus(ctx, client, rpcURL)
	return pending + queued, err
}
