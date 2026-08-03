package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleConfigsLoad keeps every shipped example loadable. They are the
// documented entry points, so a config that no longer parses (or trips runtime
// validation) is a broken quickstart.
func TestExampleConfigsLoad(t *testing.T) {
	entries, err := os.ReadDir("../../examples")
	if err != nil {
		t.Fatalf("read examples: %v", err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		found++
		t.Run(e.Name(), func(t *testing.T) {
			// CHAIN_CONFIG from the environment would override a config's own
			// profile; the examples must stand on their own.
			t.Setenv("CHAIN_CONFIG", "")
			if _, err := Load(filepath.Join("../../examples", e.Name())); err != nil {
				t.Errorf("load: %v", err)
			}
		})
	}
	if found == 0 {
		t.Fatal("no example configs found")
	}
}

// TestComparableExamplesShareOneProfile pins the settings the README's
// "Comparing chains" table depends on. The load itself is deliberately not one
// of them: full_second is a rate, and no single size suits every chain — 500,000
// never finishes on evmd, while 200,000 drains in about three seconds on
// allegro, below the window a rate can be read from. Each of these was, at some point, the
// reason a chain's measured throughput was wrong:
//
//   - a load too small to saturate the chain reports the load, not the rate;
//   - a sender that pauses at 5,000 pending measures its own refill loop;
//   - a pool that cannot hold the load turns the run into a rejection test.
func TestComparableExamplesShareOneProfile(t *testing.T) {
	names := []string{"config.local.yaml", "config.tempo.yaml", "config.allegro.yaml"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CHAIN_CONFIG", "")
			cfg, err := Load(filepath.Join("../../examples", name))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			b := cfg.Benchmark
			if b.BroadcastConcurrency != 32 {
				t.Errorf("broadcast_concurrency = %d, want 32", b.BroadcastConcurrency)
			}
			// Submission has to cost the same on all three: one round trip per
			// transaction caps the sender near 50k tx/s, which is above evmd
			// but below tempo and allegro — so mixing batch sizes silently
			// handicaps only the fast chains.
			if b.BroadcastBatchSize != 100 {
				t.Errorf("broadcast_batch_size = %d, want 100", b.BroadcastBatchSize)
			}
			// Negative never pauses and needs no relation to the load; a
			// positive value has to stay above it, which is easy to forget when
			// raising num_txs.
			if b.BroadcastPendingWatermark >= 0 &&
				b.BroadcastPendingWatermark <= int64(b.NumAccounts*b.NumTxs) {
				t.Errorf("broadcast_pending_watermark %d throttles the %d-tx load; use -1",
					b.BroadcastPendingWatermark, b.NumAccounts*b.NumTxs)
			}
			if b.Validators != 1 || b.Fullnodes != 0 {
				t.Errorf("topology = %d validators / %d fullnodes, want 1/0", b.Validators, b.Fullnodes)
			}
			if b.NumIdle != 40 {
				t.Errorf("num_idle = %d, want 40", b.NumIdle)
			}
		})
	}
}

// TestEvmdExampleDoesNotPinPoolSizes mirrors the tempo check: evmd's mempool
// capacities are derived from the load now, and a hardcoded set in app_patch
// goes stale the moment num_txs changes — which silently rejects every
// transaction past account-slots.
func TestEvmdExampleDoesNotPinPoolSizes(t *testing.T) {
	t.Setenv("CHAIN_CONFIG", "")
	cfg, err := Load("../../examples/config.local.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	evm, ok := cfg.Benchmark.AppPatch["evm"].(map[string]interface{})
	if !ok {
		return // no evm patch at all is fine
	}
	pool, ok := evm["mempool"].(map[string]interface{})
	if !ok {
		return
	}
	for _, key := range []string{"global-slots", "global-queue", "account-slots", "account-queue"} {
		if _, pinned := pool[key]; pinned {
			t.Errorf("app_patch pins evm.mempool.%s; it is derived from the load", key)
		}
	}
}
