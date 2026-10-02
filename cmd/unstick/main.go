// Command unstick clears a remote run's senders out of an RPC node's pool.
//
// When a node never forwards some transactions to the block producer, every
// later one from that sender waits on the gap, run after run. unstick sends a
// fee-token self-transfer at each nonce from the on-chain one up to the
// highest the node holds: pooled ones are replaced at a higher fee, which the
// node forwards as new, and missing ones are filled. At most -window per
// sender are in flight at once.
//
//	go run ./cmd/unstick -config /tmp/config.tempo.nvnm.yaml
package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
)

const (
	// replacementGas covers a fee-token self-transfer (26,418 used on nvnm).
	replacementGas = 40_000
	// stallRounds is how many polls a sender may sit still with replacements
	// outstanding before they are presumed lost and re-sent at a higher fee.
	stallRounds = 5
	pollEvery   = 2 * time.Second
	giveUpAfter = 2 * time.Minute
)

func main() {
	configPath := flag.String("config", "", "benchmark config with remote_rpc_url")
	gasPriceGwei := flag.Float64("gas-price", 2, "replacement fee in gwei; must beat the stuck transactions' by 10%")
	window := flag.Uint64("window", 200, "replacements per sender in flight at once")
	flag.Parse()

	cfg, err := config.LoadForGenerate(*configPath)
	if err != nil {
		fatal("load config: %v", err)
	}
	spec := cfg.Benchmark
	if spec.RemoteRPCURL == "" {
		fatal("%s has no remote_rpc_url", *configPath)
	}
	if strings.TrimSpace(spec.BaseMnemonic) == "" {
		// Empty falls back to keygen's fixed seeds: other accounts entirely.
		fatal("base_mnemonic is empty — is the environment variable the config reads it from set?")
	}

	senders := make([]*sender, spec.NumAccounts)
	for i := range senders {
		key, err := keygen.DeterministicKey(0, i+1, spec.BaseMnemonic)
		if err != nil {
			fatal("derive account %d: %v", i+1, err)
		}
		senders[i] = &sender{key: key, addr: crypto.PubkeyToAddress(key.PublicKey), price: big.NewInt(int64(*gasPriceGwei * 1e9))}
	}

	ctx := context.Background()
	client := &http.Client{Timeout: time.Duration(spec.BroadcastRequestTimeoutSeconds) * time.Second}
	signer := types.LatestSignerForChainID(big.NewInt(spec.EVMChainID))
	lastProgress, fewest := time.Now(), -1
	for {
		calls := make([]bench.RPCCall, len(senders))
		for i, s := range senders {
			calls[i] = bench.RPCCall{Method: "eth_getTransactionCount", Params: []interface{}{s.addr.Hex(), "latest"}}
		}
		counts, err := bench.JSONRPCBatch(ctx, client, spec.RemoteRPCURL, calls, spec.BroadcastBatchSize)
		if err != nil {
			fatal("read nonces: %v", err)
		}
		tops, err := pooledTops(ctx, client, spec.RemoteRPCURL)
		if err != nil {
			fatal("read the pool (txpool_inspect): %v", err)
		}

		stuck, stuckSenders, replacing := 0, 0, 0
		var streams [][]string // one per sender, in nonce order
		for i, s := range senders {
			latest, top := quantity(counts[i]), tops[s.addr]
			if top <= latest {
				continue
			}
			stuck += int(top - latest)
			stuckSenders++
			var raws []string
			for _, nonce := range s.due(latest, top, *window) {
				raw, err := s.replacement(signer, nonce)
				if err != nil {
					fatal("sign %s nonce %d: %v", s.addr.Hex(), nonce, err)
				}
				raws = append(raws, raw)
			}
			streams = append(streams, raws)
			replacing += len(raws)
		}
		if stuck == 0 {
			fmt.Println("[unstick] no transactions left in the pool for these senders")
			return
		}

		line := fmt.Sprintf("[unstick] %d stuck across %d senders; replacing %d", stuck, stuckSenders, replacing)
		if replacing > 0 {
			stats := bench.BroadcastStreams(ctx, client, spec.RemoteRPCURL, streams, bench.BroadcastOptions{
				Concurrency: spec.BroadcastConcurrency, BatchSize: spec.BroadcastBatchSize, Watermark: -1,
			})
			if stats.Rejected > 0 {
				line += fmt.Sprintf(" (rejected %d: %v)", stats.Rejected, stats.RejectReasons)
			}
			if stats.RejectReasons["replacement transaction underpriced"]+stats.RejectReasons["gas price is less than basefee"] > 0 {
				// The pool (often an earlier unstick) or the base fee asks for
				// more: outbid now rather than after stallRounds.
				for _, s := range senders {
					s.outbid()
				}
			}
		}
		fmt.Println(line)

		if fewest < 0 || stuck < fewest {
			fewest, lastProgress = stuck, time.Now()
		} else if time.Since(lastProgress) > giveUpAfter {
			fatal("no progress for %s with %d still stuck: the node may not be forwarding even new transactions", giveUpAfter, stuck)
		}
		time.Sleep(pollEvery)
	}
}

// sender tracks one account's replacements.
type sender struct {
	key        *ecdsa.PrivateKey
	addr       common.Address
	price      *big.Int
	next       uint64 // lowest nonce not yet replaced at the current price
	lastLatest uint64
	stalled    int
}

// pooledTops returns, per sender, one past the highest nonce the node holds,
// queued included: every nonce below it is pooled or missing, and either way
// needs a transaction before the rest can land.
func pooledTops(ctx context.Context, client *http.Client, url string) (map[common.Address]uint64, error) {
	var pool struct {
		Pending map[string]map[string]json.RawMessage `json:"pending"`
		Queued  map[string]map[string]json.RawMessage `json:"queued"`
	}
	if err := bench.JSONRPCCall(ctx, client, url, "txpool_inspect", []interface{}{}, &pool); err != nil {
		return nil, err
	}
	tops := map[common.Address]uint64{}
	for _, sub := range []map[string]map[string]json.RawMessage{pool.Pending, pool.Queued} {
		for addr, byNonce := range sub {
			a := common.HexToAddress(addr)
			for n := range byNonce {
				if v, err := strconv.ParseUint(n, 10, 64); err == nil && v+1 > tops[a] {
					tops[a] = v + 1
				}
			}
		}
	}
	return tops, nil
}

// due returns the nonces to send this round, given the account's on-chain
// nonce and one past the highest the node holds for it.
func (s *sender) due(latest, pending, window uint64) []uint64 {
	switch {
	case latest > s.lastLatest:
		s.lastLatest, s.stalled = latest, 0
	case s.next > latest:
		if s.stalled++; s.stalled >= stallRounds {
			// These replacements never reached the producer either.
			s.outbid()
		}
	}
	s.next = max(s.next, latest)
	var nonces []uint64
	for ; s.next < pending && s.next < latest+window; s.next++ {
		nonces = append(nonces, s.next)
	}
	return nonces
}

// outbid re-sends everything outstanding for 25% more, which the node takes as
// new transactions and forwards.
func (s *sender) outbid() {
	s.price = new(big.Int).Div(new(big.Int).Mul(s.price, big.NewInt(5)), big.NewInt(4))
	s.next, s.stalled = s.lastLatest, 0
}

func (s *sender) replacement(signer types.Signer, nonce uint64) (string, error) {
	tx, err := types.SignTx(types.NewTx(&types.DynamicFeeTx{
		ChainID:   signer.ChainID(),
		Nonce:     nonce,
		GasTipCap: s.price,
		GasFeeCap: s.price,
		Gas:       replacementGas,
		To:        &tempotx.FeeToken,
		Data:      tempotx.Transfer(s.addr, 1),
	}), signer, s.key)
	if err != nil {
		return "", err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", err
	}
	return hexutil.Encode(raw), nil
}

func quantity(raw json.RawMessage) uint64 {
	var s string
	_ = json.Unmarshal(raw, &s)
	v, err := hexutil.DecodeUint64(strings.TrimSpace(s))
	if err != nil {
		fatal("unexpected nonce %s", raw)
	}
	return v
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "FAIL: "+format+"\n", args...)
	os.Exit(1)
}
