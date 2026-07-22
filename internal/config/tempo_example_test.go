package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTempoExampleConfigParses(t *testing.T) {
	cfg, err := Load("../../examples/config.tempo.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	if b.ChainFamily != "tempo" {
		t.Errorf("chain_family = %q", b.ChainFamily)
	}
	// tempo / tempo-xtask default to PATH command names.
	if b.TempoBin == "" || b.TempoXtaskBin == "" {
		t.Errorf("tempo binaries not parsed: bin=%q xtask=%q", b.TempoBin, b.TempoXtaskBin)
	}
	if b.TempoBasePort != 8000 || b.TempoGasLimit != 3000000000 {
		t.Errorf("tempo ports/gas not parsed: port=%d gas=%d", b.TempoBasePort, b.TempoGasLimit)
	}
	if b.TxType != "erc20-transfer" || b.ERC20TransferGas != 300000 {
		t.Errorf("tx settings not parsed: %s %d", b.TxType, b.ERC20TransferGas)
	}
	t.Logf("parsed OK: accounts=%d validators=%d", b.NumAccounts, b.Validators)
}

func TestTempoDockerExampleConfigParses(t *testing.T) {
	cfg, err := Load("../../examples/config.tempo.docker.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	if b.ChainFamily != "tempo" || b.RunnerType != "docker" {
		t.Errorf("family/runner = %q/%q", b.ChainFamily, b.RunnerType)
	}
	// Compose owns the container lifecycle; the benchmark must not start nodes.
	if b.StartNode {
		t.Error("start_node must be false in docker mode")
	}
	// The docker launcher derives trusted peers from the OTHER validators.
	if b.Validators < 2 {
		t.Errorf("docker needs >=2 validators, got %d", b.Validators)
	}
	if b.TempoDockerImage == "" || b.TempoComposeProject == "" {
		t.Errorf("image/project not parsed: %q %q", b.TempoDockerImage, b.TempoComposeProject)
	}
	// The stop script looks the project up by this fixed name.
	if b.TempoComposeProject != "evm-benchmark-tempo" {
		t.Errorf("compose project %q must match scripts/run-benchmark.sh", b.TempoComposeProject)
	}
	// data_dir uses ~ in the example; Load must expand it to a real host path.
	if b.DataDir == "" || b.DataDir[0] == '~' {
		t.Errorf("data_dir not expanded: %q", b.DataDir)
	}
}

// TestTempoConfigDoesNotInheritCosmosToken guards a silent-wrongness path: the
// cosmos ERC-20 default must not be applied to a tempo config, or every
// transaction would target a contract that does not exist on Tempo.
func TestTempoConfigDoesNotInheritCosmosToken(t *testing.T) {
	cfg, err := Load("../../examples/config.tempo.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Benchmark.ERC20ContractAddress; got == "0x1000000000000000000000000000000000000000" {
		t.Errorf("tempo config inherited the cosmos ERC-20 default: %s", got)
	}
}

// TestTempoConfigDefaultsResolveThroughRuntime checks the full path a real run
// takes: config.Load must not pre-fill cosmos-shaped defaults that the tempo
// runtime is supposed to supply, or its own defaults become unreachable.
func TestTempoConfigDefaultsResolveThroughRuntime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "minimal.yaml")
	// Deliberately omits tx_type and erc20_contract_address.
	minimal := `benchmark:
  chain_family: tempo
  runner_type: local
  data_dir: /tmp/x
  evm_chain_id: 1337
  erc20_transfer_gas: 300000
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
	// The cosmos default (simple-transfer) would be rejected by Tempo, so the
	// family default must be the ERC-20/TIP-20 shape instead.
	if got := cfg.Benchmark.TxType; got != "erc20-transfer" {
		t.Errorf("tx_type = %q, want erc20-transfer for tempo", got)
	}
	if got := cfg.Benchmark.ERC20ContractAddress; got != "" {
		t.Errorf("erc20_contract_address = %q; tempo must be left to default it to its TIP-20 fee token", got)
	}
}
