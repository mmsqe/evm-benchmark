package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
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

func BroadcastRawTxs(ctx context.Context, client *http.Client, rpcURL string, txs []string, concurrency int) {
	if concurrency < 1 {
		concurrency = 1
	}

	jobs := make(chan string)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for raw := range jobs {
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
