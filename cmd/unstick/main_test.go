package main

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestSenderDue pins the pacing: a window of replacements from the on-chain
// nonce, sliding as the chain advances, re-sent at a higher fee when the
// chain stops advancing, and nothing once the pool is empty.
func TestSenderDue(t *testing.T) {
	s := &sender{price: big.NewInt(2e9)}
	span := func(nonces []uint64) string {
		if len(nonces) == 0 {
			return "none"
		}
		return fmt.Sprintf("%d..%d", nonces[0], nonces[len(nonces)-1])
	}

	if got := span(s.due(100, 1000, 200)); got != "100..299" {
		t.Fatalf("first round = %s, want 100..299", got)
	}
	if got := span(s.due(250, 1000, 200)); got != "300..449" {
		t.Fatalf("after the chain advanced = %s, want 300..449", got)
	}
	for i := 1; i < stallRounds; i++ {
		if got := span(s.due(250, 1000, 200)); got != "none" {
			t.Fatalf("stalled round %d = %s, want to wait", i, got)
		}
	}
	if got := span(s.due(250, 1000, 200)); got != "250..449" || s.price.Cmp(big.NewInt(25e8)) != 0 {
		t.Fatalf("after %d stalled rounds = %s at %s, want 250..449 re-sent at 2.5 gwei", stallRounds, got, s.price)
	}
	if got := span(s.due(1000, 1000, 200)); got != "none" {
		t.Fatalf("empty pool = %s, want none", got)
	}
}

// TestSenderOutbid: when the node calls a replacement underpriced (an earlier
// run's replacements are pooled), the next round re-sends at 25% more at once
// rather than after stallRounds.
func TestSenderOutbid(t *testing.T) {
	s := &sender{price: big.NewInt(2e9)}
	s.due(100, 1000, 200)
	s.outbid()
	got := s.due(100, 1000, 200)
	if len(got) != 200 || got[0] != 100 || s.price.Cmp(big.NewInt(25e8)) != 0 {
		t.Errorf("after outbid: %d nonces from %v at %s, want 200 from 100 at 2.5 gwei", len(got), got[:1], s.price)
	}
}

func TestReplacementIsSignedBySender(t *testing.T) {
	key, _ := crypto.GenerateKey()
	s := &sender{key: key, addr: crypto.PubkeyToAddress(key.PublicKey), price: big.NewInt(2e9)}
	signer := types.LatestSignerForChainID(big.NewInt(787222))

	raw, err := s.replacement(signer, 369)
	if err != nil {
		t.Fatal(err)
	}
	var tx types.Transaction
	if err := tx.UnmarshalBinary(hexutil.MustDecode(raw)); err != nil {
		t.Fatal(err)
	}
	from, err := types.Sender(signer, &tx)
	if err != nil || from != s.addr || tx.Nonce() != 369 || tx.GasFeeCap().Cmp(s.price) != 0 {
		t.Errorf("from=%s nonce=%d fee=%s, want %s 369 %s", from.Hex(), tx.Nonce(), tx.GasFeeCap(), s.addr.Hex(), s.price)
	}
}

// TestPooledTopsSeesPastGaps: queued transactions above a missing nonce count,
// or unstick would stop at the gap and leave them stranded in the pool.
func TestPooledTopsSeesPastGaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{
			"pending":{"0x5b156340D86a69172416bD00255a9f1b899fC983":{"3387":"x","3397":"x"}},
			"queued":{"0x5b156340d86a69172416bd00255a9f1b899fc983":{"3989":"x","4233":"x"},
			          "0x04056CB5641A598F1156227005BB8F42ff93FFe4":{"3988":"x"}}}}`)
	}))
	defer srv.Close()

	tops, err := pooledTops(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got := tops[common.HexToAddress("0x5b156340D86a69172416bD00255a9f1b899fC983")]; got != 4234 {
		t.Errorf("top = %d, want 4234 (past the queued ones)", got)
	}
	if got := tops[common.HexToAddress("0x04056CB5641A598F1156227005BB8F42ff93FFe4")]; got != 3989 {
		t.Errorf("queued-only sender top = %d, want 3989", got)
	}
}
