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
			if raws[0] == "0xbad" {
				return nil, "nonce too low"
			}
			return "0xHASH-" + raws[0], ""
		}
		return nil, "unexpected " + method
	})

	txs := make([]string, 600)
	for i := range txs {
		txs[i] = fmt.Sprintf("0x%03d", i)
	}
	txs[5] = "0xbad"
	stats := BroadcastRawTxs(context.Background(), srv.Client(), srv.URL, txs, 1, -1, 100, true)

	if stats.Rejected != 1 {
		t.Errorf("rejected = %d, want 1", stats.Rejected)
	}
	if len(stats.Accepted) != 599 {
		t.Errorf("accepted = %d hashes, want 599", len(stats.Accepted))
	}
	if _, ok := stats.Accepted["0xhash-0x000"]; !ok {
		t.Error("accepted set is missing the node-returned (lower-cased) hash")
	}
	if !stats.PoolUnknown || stats.Undersaturated() {
		t.Errorf("PoolUnknown=%v Undersaturated=%v, want an unknown pool and no saturation verdict",
			stats.PoolUnknown, stats.Undersaturated())
	}

	untracked := BroadcastRawTxs(context.Background(), srv.Client(), srv.URL, txs[:10], 1, -1, 5, false)
	if untracked.Accepted != nil {
		t.Error("hashes collected without being asked for")
	}
}
