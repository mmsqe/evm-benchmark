package config

import (
	"testing"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// TestRethValidationExampleConfig pins the A/B example's settings that would
// silently change what a run measures.
func TestRethValidationExampleConfig(t *testing.T) {
	cfg, err := Load("../../examples/config.reth.validation.jsonnet")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	// A fullnode is what a validation change is compared on.
	if b.ChainFamily != "reth" || b.Validators != 1 || b.Fullnodes != 1 {
		t.Errorf("family/validators/fullnodes = %q/%d/%d, want reth/1/1", b.ChainFamily, b.Validators, b.Fullnodes)
	}
	// A throttled sender would be measured instead of the chain.
	if b.BroadcastPendingWatermark >= 0 {
		t.Errorf("broadcast_pending_watermark = %d, want -1 (never pause)", b.BroadcastPendingWatermark)
	}
	// The token comes from the reth genesis, not the cosmos default.
	if b.TxType != messages.ERC20TransferTx || b.ERC20ContractAddress != "" {
		t.Errorf("tx_type/erc20_contract_address = %q/%q, want erc20-transfer with no inherited token",
			b.TxType, b.ERC20ContractAddress)
	}
	if !b.ValidatorGenerateLoad {
		t.Error("validator_generate_load must be true, or the producer takes no load")
	}
	// It imports config.reth.jsonnet, whose intake tuning and Osaka lift would
	// change what the A/B runs.
	if len(b.RethNodeArgs) != 0 || b.GenesisPatch != nil {
		t.Errorf("reth_node_args/genesis_patch = %v/%v, want neither inherited", b.RethNodeArgs, b.GenesisPatch)
	}
}

// TestRethExampleConfig pins what makes the default example a throughput
// ceiling: one node taking a load long enough to measure, a gas limit no block
// fills, and no Osaka block size cap.
func TestRethExampleConfig(t *testing.T) {
	cfg, err := Load("../../examples/config.reth.jsonnet")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	// A fullnode's share of the load reaches the producer by gossip, which is
	// slower than the blocks are.
	if b.ChainFamily != "reth" || b.Validators != 1 || b.Fullnodes != 0 {
		t.Errorf("family/validators/fullnodes = %q/%d/%d, want reth/1/0", b.ChainFamily, b.Validators, b.Fullnodes)
	}
	// 500,000 drained in four blocks, too few whole seconds for a rate.
	if load := b.NumAccounts * b.NumTxs; load < 2_000_000 {
		t.Errorf("load = %d transactions, want at least 2,000,000", load)
	}
	// Fed fast enough, one block carried 243k transfers, past 3e9 gas.
	if b.RethGasLimit < 10_000_000_000 || b.BroadcastPendingWatermark >= 0 {
		t.Errorf("gas limit/watermark = %d/%d, want at least 1e10 and a sender that never pauses",
			b.RethGasLimit, b.BroadcastPendingWatermark)
	}
	config, _ := b.GenesisPatch["config"].(map[string]interface{})
	if v, ok := config["osakaTime"]; !ok || v != nil {
		t.Errorf("genesis_patch.config.osakaTime = %v, want null: Osaka's block size cap binds at one block a second", v)
	}
}

// TestHotRecipientPercentValidation pins where hot_recipient_percent applies:
// the shared legacy signer, at a share between 0 and 100.
func TestHotRecipientPercentValidation(t *testing.T) {
	base := AppConfig{Benchmark: messages.BenchmarkSpec{
		DataDir:    "/tmp/x",
		Validators: 1,
		TxType:     messages.SimpleTransferTx,
		RunnerType: "local",
	}}
	for name, tc := range map[string]struct {
		mutate func(*messages.BenchmarkSpec)
		ok     bool
	}{
		"reth":         {func(s *messages.BenchmarkSpec) { s.ChainFamily = "reth"; s.HotRecipientPercent = 30 }, true},
		"over 100":     {func(s *messages.BenchmarkSpec) { s.HotRecipientPercent = 101 }, false},
		"negative":     {func(s *messages.BenchmarkSpec) { s.HotRecipientPercent = -1 }, false},
		"tempo native": {func(s *messages.BenchmarkSpec) { s.ChainFamily = "tempo"; s.HotRecipientPercent = 30 }, false},
		"tempo legacy": {func(s *messages.BenchmarkSpec) {
			s.ChainFamily = "tempo"
			s.TempoLegacyTxs = true
			s.HotRecipientPercent = 30
		}, true},
		"tempo native 0": {func(s *messages.BenchmarkSpec) { s.ChainFamily = "tempo" }, true},
	} {
		cfg := base
		tc.mutate(&cfg.Benchmark)
		if err := validate(cfg, false); (err == nil) != tc.ok {
			t.Errorf("%s: validate = %v, want ok=%v", name, err, tc.ok)
		}
	}
}
