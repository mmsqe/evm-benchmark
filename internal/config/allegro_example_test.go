package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllegroExampleConfigParses(t *testing.T) {
	cfg, err := Load("../../examples/config.allegro.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	if b.ChainFamily != "allegro" || b.RunnerType != "local" {
		t.Errorf("family/runner = %q/%q", b.ChainFamily, b.RunnerType)
	}
	// allegro / allegro-xtask default to PATH command names.
	if b.AllegroBin == "" || b.AllegroXtaskBin == "" {
		t.Errorf("allegro binaries not parsed: bin=%q xtask=%q", b.AllegroBin, b.AllegroXtaskBin)
	}
	// 3e9 matches the tempo/evmd examples so gas is non-binding on all three
	// (see "Comparing chains" in the README).
	if b.AllegroBasePort != 9000 || b.AllegroGasLimit != 3000000000 {
		t.Errorf("ports/gas not parsed: port=%d gas=%d", b.AllegroBasePort, b.AllegroGasLimit)
	}
	// The sender must not throttle, or a fast chain is measured on the
	// generator's refill loop instead of its own execution.
	if b.BroadcastPendingWatermark >= 0 {
		t.Errorf("broadcast_pending_watermark = %d, want -1 (never pause)", b.BroadcastPendingWatermark)
	}
	if b.AllegroLeaderTimeoutMS != 1000 || b.AllegroCertTimeoutMS != 2000 {
		t.Errorf("consensus timeouts not parsed: %d/%d", b.AllegroLeaderTimeoutMS, b.AllegroCertTimeoutMS)
	}
	// Allegro is a stock reth EVM, so native transfers are the workload.
	if b.TxType != "simple-transfer" {
		t.Errorf("tx_type = %q, want simple-transfer", b.TxType)
	}
	// Every node is a validator, so nothing generates load unless this is set.
	if !b.ValidatorGenerateLoad {
		t.Error("validator_generate_load must be true: an allegro devnet is validators only")
	}
}

// TestAllegroConfigDoesNotInheritCosmosToken guards a silent-wrongness path: the
// cosmos ERC-20 default must not be applied to an allegro config, where no
// contract exists at that address.
func TestAllegroConfigDoesNotInheritCosmosToken(t *testing.T) {
	cfg, err := Load("../../examples/config.allegro.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Benchmark.ERC20ContractAddress; got != "" {
		t.Errorf("allegro config inherited an ERC-20 default: %s", got)
	}
}

// TestAllegroConfigIgnoresChainProfile guards against an inherited CHAIN_CONFIG
// from the environment rewriting an allegro run's chain id and binary with a
// cosmos profile's.
func TestAllegroConfigIgnoresChainProfile(t *testing.T) {
	t.Setenv("CHAIN_CONFIG", "evmd")
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.yaml")
	minimal := `benchmark:
  chain_family: allegro
  runner_type: local
  data_dir: /tmp/x
  evm_chain_id: 1337
  num_accounts: 1
  num_txs: 1
`
	if err := os.WriteFile(path, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Benchmark.ChainConfig != "" {
		t.Errorf("chain_config = %q; cosmos profiles must not apply to allegro", cfg.Benchmark.ChainConfig)
	}
	if got := cfg.Benchmark.EVMChainID; got != 1337 {
		t.Errorf("evm_chain_id = %d, want the config's own 1337", got)
	}
}
