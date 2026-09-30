package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// rpcHandler answers one JSON-RPC call: a result, or an error message.
type rpcHandler func(method string, params json.RawMessage) (interface{}, string)

// fakeRPC serves JSON-RPC over HTTP, single and batched, from handle.
func fakeRPC(t *testing.T, handle rpcHandler) *httptest.Server {
	t.Helper()
	answer := func(req map[string]json.RawMessage) map[string]interface{} {
		var method string
		_ = json.Unmarshal(req["method"], &method)
		result, errMsg := handle(method, req["params"])
		resp := map[string]interface{}{"jsonrpc": "2.0", "id": req["id"]}
		if errMsg != "" {
			resp["error"] = map[string]interface{}{"code": -32000, "message": errMsg}
		} else {
			resp["result"] = result
		}
		return resp
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
			var reqs []map[string]json.RawMessage
			_ = json.Unmarshal(body, &reqs)
			out := make([]map[string]interface{}, len(reqs))
			for i, req := range reqs {
				out[i] = answer(req)
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		var req map[string]json.RawMessage
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(answer(req))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPostRPCWaitsOutRateLimit pins the public-endpoint path: a 429 is waited
// out and retried rather than surfacing as a failed call, which in a batch
// send would silently drop every transaction in it.
func TestPostRPCWaitsOutRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x10"}`)
	}))
	defer srv.Close()

	h, err := CurrentHeight(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("CurrentHeight: %v", err)
	}
	if h != 16 || calls.Load() != 3 {
		t.Errorf("height=%d after %d requests, want 16 after 3", h, calls.Load())
	}
}

// TestDecodeErrorNamesHTTPStatus: a public gateway refuses an oversized batch
// with 413 and a single error object, which must read as that, not as a node
// that cannot batch.
func TestDecodeErrorNamesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"request too large"}}`)
	}))
	defer srv.Close()

	_, err := JSONRPCBatch(context.Background(), srv.Client(), srv.URL,
		[]RPCCall{{Method: "eth_blockNumber"}, {Method: "eth_blockNumber"}}, 10)
	if err == nil || !strings.Contains(err.Error(), "413") {
		t.Fatalf("err = %v, want it to name HTTP 413", err)
	}
}

// TestJSONRPCBatchChunksAndOrders pins the read helper: requests stay within
// the endpoint's batch cap, and results come back in call order even when the
// server answers a batch out of order.
func TestJSONRPCBatchChunksAndOrders(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var reqs []struct {
			ID     int      `json:"id"`
			Params []string `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		mu.Lock()
		sizes = append(sizes, len(reqs))
		mu.Unlock()
		out := make([]map[string]interface{}, 0, len(reqs))
		for i := len(reqs) - 1; i >= 0; i-- {
			out = append(out, map[string]interface{}{"jsonrpc": "2.0", "id": reqs[i].ID, "result": "echo-" + reqs[i].Params[0]})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	calls := make([]RPCCall, 7)
	for i := range calls {
		calls[i] = RPCCall{Method: "echo", Params: []string{fmt.Sprint(i)}}
	}
	results, err := JSONRPCBatch(context.Background(), srv.Client(), srv.URL, calls, 3)
	if err != nil {
		t.Fatalf("JSONRPCBatch: %v", err)
	}
	for i, r := range results {
		if want := fmt.Sprintf(`"echo-%d"`, i); string(r) != want {
			t.Errorf("result %d = %s, want %s", i, r, want)
		}
	}
	if fmt.Sprint(sizes) != "[3 3 1]" {
		t.Errorf("batch sizes = %v, want [3 3 1]", sizes)
	}
}

func TestJSONRPCBatchFailsOnCallError(t *testing.T) {
	srv := fakeRPC(t, func(method string, _ json.RawMessage) (interface{}, string) {
		if method == "bad" {
			return nil, "nope"
		}
		return "0x1", ""
	})
	_, err := JSONRPCBatch(context.Background(), srv.Client(), srv.URL,
		[]RPCCall{{Method: "ok"}, {Method: "bad"}}, 10)
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("err = %v, want the failing call named", err)
	}
}

// TestWatermarkIgnoresQueued pins the deadlock fix: queued transactions wait
// on nonces a paused sender may be holding, so a pool deep only in queued ones
// must not keep the sender paused.
func TestWatermarkIgnoresQueued(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"pending":"0x10","queued":"0x100000"}}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var deepest atomic.Int64
	var stalled atomic.Bool
	if !awaitPoolRoom(ctx, srv.Client(), srv.URL, 100, &deepest, &stalled) {
		t.Fatal("paused on queued transactions: a sender holding their missing nonces would never resume")
	}
	if deepest.Load() != 16 {
		t.Errorf("recorded depth %d, want the 16 executable ones", deepest.Load())
	}
}

// TestWatermarkGivesUpOnStuckPool: transactions a node never forwards stay
// pending forever, so a pool that stops draining must end the wait, and no
// sender waits on it again.
func TestWatermarkGivesUpOnStuckPool(t *testing.T) {
	defer func(d time.Duration) { poolStallTimeout = d }(poolStallTimeout)
	poolStallTimeout = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"pending":"0xc42","queued":"0x0"}}`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var deepest atomic.Int64
	var stalled atomic.Bool
	if !awaitPoolRoom(ctx, srv.Client(), srv.URL, 3000, &deepest, &stalled) || !stalled.Load() {
		t.Fatal("kept waiting on a pool stuck above the watermark")
	}
	start := time.Now()
	awaitPoolRoom(ctx, srv.Client(), srv.URL, 3000, &deepest, &stalled)
	if time.Since(start) > 100*time.Millisecond {
		t.Error("waited again after the pool was found stuck")
	}
}

// TestBroadcastTracksAcceptedHashes pins what a shared-chain run is built on:
// the hashes the node returned, rejected transactions left out, and a pool the
// endpoint will not report flagged as unknown rather than read as empty.
func TestBroadcastTracksAcceptedHashes(t *testing.T) {
	srv := fakeRPC(t, func(method string, params json.RawMessage) (interface{}, string) {
		switch method {
		case "txpool_status":
			return nil, "method not allowed: txpool_status"
		case "eth_sendRawTransaction":
			var raws []string
			_ = json.Unmarshal(params, &raws)
			if strings.HasPrefix(raws[0], "0xbad") {
				return nil, "nonce too low: next nonce 7, tx nonce " + raws[0][5:]
			}
			return "0xHASH-" + raws[0], ""
		}
		return nil, "unexpected " + method
	})

	txs := make([]string, 600)
	for i := range txs {
		txs[i] = fmt.Sprintf("0x%03d", i)
	}
	txs[5], txs[6] = "0xbad3", "0xbad4"
	stats := BroadcastRawTxs(context.Background(), srv.Client(), srv.URL, txs,
		BroadcastOptions{Concurrency: 1, BatchSize: 100, Watermark: -1, TrackAccepted: true})

	// Two rejections that differ only in their numbers are one reason.
	if want := map[string]int{"nonce too low: next nonce N, tx nonce N": 2}; stats.Rejected != 2 || fmt.Sprint(stats.RejectReasons) != fmt.Sprint(want) {
		t.Errorf("rejected = %d %v, want 2 %v", stats.Rejected, stats.RejectReasons, want)
	}
	if len(stats.Accepted) != 598 {
		t.Errorf("accepted = %d hashes, want 598", len(stats.Accepted))
	}
	if _, ok := stats.Accepted["0xhash-0x000"]; !ok {
		t.Error("accepted set is missing the node-returned (lower-cased) hash")
	}
	if !stats.PoolUnknown || stats.Undersaturated() {
		t.Errorf("PoolUnknown=%v Undersaturated=%v, want an unknown pool and no saturation verdict",
			stats.PoolUnknown, stats.Undersaturated())
	}

	untracked := BroadcastRawTxs(context.Background(), srv.Client(), srv.URL, txs[:10],
		BroadcastOptions{Concurrency: 1, BatchSize: 5, Watermark: -1})
	if untracked.Accepted != nil {
		t.Error("hashes collected without being asked for")
	}
}

// TestBroadcastStreamsKeepsEachStreamInOrder pins what a remote run relies on:
// with many workers in flight, no transaction of an account reaches the node
// before the one ahead of it, and every one is sent.
func TestBroadcastStreamsKeepsEachStreamInOrder(t *testing.T) {
	var mu sync.Mutex
	arrived := map[string][]int{}
	srv := fakeRPC(t, func(method string, params json.RawMessage) (interface{}, string) {
		var raws []string
		_ = json.Unmarshal(params, &raws)
		var account string
		var nonce int
		_, _ = fmt.Sscanf(strings.Replace(raws[0], "-", " ", 1), "%s %d", &account, &nonce)
		mu.Lock()
		arrived[account] = append(arrived[account], nonce)
		mu.Unlock()
		time.Sleep(time.Duration(nonce%3) * time.Millisecond) // uneven replies reorder unordered sends
		return "0xhash", ""
	})

	streams := make([][]string, 8)
	for a := range streams {
		for n := 0; n < 50; n++ {
			streams[a] = append(streams[a], fmt.Sprintf("0x%x-%d", a, n))
		}
	}
	stats := BroadcastStreams(context.Background(), srv.Client(), srv.URL, streams,
		BroadcastOptions{Concurrency: 4, BatchSize: 10, Watermark: -1})

	if stats.Sent != 400 {
		t.Errorf("sent %d, want 400", stats.Sent)
	}
	for account, nonces := range arrived {
		if len(nonces) != 50 {
			t.Errorf("account %s: %d arrived, want 50", account, len(nonces))
		}
		for i, n := range nonces {
			if n != i {
				t.Fatalf("account %s: nonce %d arrived in position %d", account, n, i)
			}
		}
	}
}
