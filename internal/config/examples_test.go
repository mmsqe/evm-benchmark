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
// "Comparing chains" table depends on. Each of these was, at some point, the
// reason a chain's measured throughput was wrong:
//
//   - a load too small to saturate the chain reports the load, not the rate;
//   - a sender that pauses at 5,000 pending measures its own refill loop;
//   - a pool that cannot hold the load turns the run into a rejection test.
func TestComparableExamplesShareOneProfile(t *testing.T) {
	const (
		wantAccounts = 2000
		wantTxs      = 100
	)
	for _, name := range []string{"config.local.yaml", "config.tempo.yaml", "config.allegro.yaml"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CHAIN_CONFIG", "")
			cfg, err := Load(filepath.Join("../../examples", name))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			b := cfg.Benchmark
			if b.NumAccounts != wantAccounts || b.NumTxs != wantTxs {
				t.Errorf("load = %d x %d, want %d x %d (the three must match to be comparable)",
					b.NumAccounts, b.NumTxs, wantAccounts, wantTxs)
			}
			if b.BroadcastConcurrency != 32 {
				t.Errorf("broadcast_concurrency = %d, want 32", b.BroadcastConcurrency)
			}
			if b.BroadcastPendingWatermark <= int64(b.NumAccounts*b.NumTxs) {
				t.Errorf("broadcast_pending_watermark %d must exceed the load %d, or the sender throttles",
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

// TestEvmdExamplePoolHoldsTheLoad guards the trap that made an unthrottled evmd
// run drop three quarters of its load: CometBFT's mempool.size is inert while
// mempool.type is "app", so the capacity that matters lives in app.toml under
// evm.mempool — and its defaults (global-slots 5120, global-queue 1024,
// account-slots 16) hold ~6k transactions.
func TestEvmdExamplePoolHoldsTheLoad(t *testing.T) {
	t.Setenv("CHAIN_CONFIG", "")
	cfg, err := Load("../../examples/config.local.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	evm, ok := b.AppPatch["evm"].(map[string]interface{})
	if !ok {
		t.Fatalf("app_patch.evm missing: %v", b.AppPatch)
	}
	pool, ok := evm["mempool"].(map[string]interface{})
	if !ok {
		t.Fatalf("app_patch.evm.mempool missing: %v", evm)
	}
	load := b.NumAccounts * b.NumTxs
	for _, key := range []string{"global-slots", "global-queue"} {
		got, ok := pool[key].(int)
		if !ok || got < load {
			t.Errorf("evm.mempool.%s = %v, must hold the whole %d-tx load", key, pool[key], load)
		}
	}
	if got, ok := pool["account-slots"].(int); !ok || got < b.NumTxs {
		t.Errorf("evm.mempool.account-slots = %v, want >= num_txs (%d)", pool["account-slots"], b.NumTxs)
	}
}
