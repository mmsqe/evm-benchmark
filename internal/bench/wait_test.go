package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// sharedChain is a fake public chain: every block carries other users'
// transactions, and ours land where the test puts them. The head advances one
// block per eth_blockNumber call.
type sharedChain struct {
	mu   sync.Mutex
	head int64
	ours map[int64][]string
}

func (c *sharedChain) handle(method string, params json.RawMessage) (interface{}, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch method {
	case "eth_blockNumber":
		c.head++
		return fmt.Sprintf("0x%x", c.head), ""
	case "eth_getBlockByNumber":
		var p []interface{}
		_ = json.Unmarshal(params, &p)
		var h int64
		_, _ = fmt.Sscanf(p[0].(string), "0x%x", &h)
		if h > c.head {
			return nil, "not yet"
		}
		txs := []string{fmt.Sprintf("0xother-%d-a", h), fmt.Sprintf("0xother-%d-b", h)}
		txs = append(txs, c.ours[h]...)
		return map[string]interface{}{"timestamp": fmt.Sprintf("0x%x", 1000+h), "transactions": txs}, ""
	}
	return nil, "unexpected " + method
}

func ownSet(hashes ...string) TxSet {
	s := TxSet{}
	for _, h := range hashes {
		s.add(h)
	}
	return s
}

// TestWaitForTxsReturnsWhenAllIncluded: other users' transactions never let a
// shared chain look idle, so the wait ends on this run's own being included.
func TestWaitForTxsReturnsWhenAllIncluded(t *testing.T) {
	chain := &sharedChain{head: 100, ours: map[int64][]string{102: {"0xA"}, 104: {"0xB", "0xC"}}}
	srv := fakeRPC(t, chain.handle)

	done := make(chan error, 1)
	go func() {
		done <- WaitForTxs(context.Background(), srv.Client(), srv.URL, ownSet("0xa", "0xb", "0xc"),
			101, 1000, time.Millisecond, time.Minute)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForTxs: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForTxs never returned although every transaction was included")
	}
	if chain.head > 110 {
		t.Errorf("kept following the chain to %d after the last inclusion at 104", chain.head)
	}
}

// TestWaitForTxsGivesUpOnDroppedTxs: a transaction that never lands must not
// hold the run open forever; idleBlocks blocks without any of ours ends it.
func TestWaitForTxsGivesUpOnDroppedTxs(t *testing.T) {
	chain := &sharedChain{head: 100, ours: map[int64][]string{103: {"0xA"}}}
	srv := fakeRPC(t, chain.handle)

	done := make(chan error, 1)
	go func() {
		done <- WaitForTxs(context.Background(), srv.Client(), srv.URL, ownSet("0xa", "0xdropped"),
			101, 5, time.Millisecond, time.Minute)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForTxs: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WaitForTxs waited forever for a dropped transaction")
	}
	if chain.head < 108 {
		t.Errorf("gave up at head %d, before 5 quiet blocks after the last inclusion", chain.head)
	}
}

// TestDumpBlockStatsCountsOnlyOwn: on a shared chain the other users'
// transactions in the same blocks must not be reported as this run's.
func TestDumpBlockStatsCountsOnlyOwn(t *testing.T) {
	chain := &sharedChain{head: 200, ours: map[int64][]string{102: {"0xA", "0xB"}, 103: {"0xC"}}}
	srv := fakeRPC(t, chain.handle)

	var out bytes.Buffer
	stats, err := DumpBlockStats(context.Background(), &out, srv.Client(), srv.URL, 101, 105, 4, ownSet("0xa", "0xb", "0xc", "0xd"))
	if err != nil {
		t.Fatalf("DumpBlockStats: %v", err)
	}
	if stats.IncludedTxs != 3 {
		t.Errorf("included = %d, want 3 (only ours)", stats.IncludedTxs)
	}
	if !strings.Contains(out.String(), "tx_summary sent=4 included=3 missing=1 non_empty_blocks=2") {
		t.Errorf("unexpected summary:\n%s", out.String())
	}
}
