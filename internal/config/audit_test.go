package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditExamples is a report, not an assertion: it prints the knobs that
// decide whether two chains' numbers can go in one table.
func TestAuditExamples(t *testing.T) {
	if os.Getenv("AUDIT") == "" {
		t.Skip("set AUDIT=1")
	}
	names := []string{"config.local.yaml", "config.yaml", "config.tempo.yaml", "config.tempo.docker.yaml", "config.allegro.yaml"}
	fmt.Printf("%-26s %-7s %6s %5s %9s %5s %5s %8s %5s %5s %12s %s\n",
		"config", "runner", "accts", "txs", "load", "conc", "batch", "waterm", "idle", "vals", "block_gas", "tx_type")
	for _, n := range names {
		t.Setenv("CHAIN_CONFIG", "")
		cfg, err := Load(filepath.Join("../../examples", n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		b := cfg.Benchmark
		gas := "-"
		switch {
		case b.TempoGasLimit > 0:
			gas = fmt.Sprintf("%d", b.TempoGasLimit)
		case b.AllegroGasLimit > 0:
			gas = fmt.Sprintf("%d", b.AllegroGasLimit)
		default:
			if gp, ok := b.GenesisPatch["consensus"].(map[string]interface{}); ok {
				if p, ok := gp["params"].(map[string]interface{}); ok {
					if blk, ok := p["block"].(map[string]interface{}); ok {
						gas = fmt.Sprintf("%v", blk["max_gas"])
					}
				}
			}
		}
		batch := b.BroadcastBatchSize
		if batch < 1 {
			batch = 1
		}
		fmt.Printf("%-26s %-7s %6d %5d %9d %5d %5d %8d %5d %5d %12s %s\n",
			n, b.RunnerType, b.NumAccounts, b.NumTxs, b.NumAccounts*b.NumTxs,
			b.BroadcastConcurrency, batch, b.BroadcastPendingWatermark, b.NumIdle,
			b.Validators, strings.Trim(gas, `"`), b.TxType)
	}
}
