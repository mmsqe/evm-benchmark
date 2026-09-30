package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestModeratoExampleConfig pins the public-testnet example to what the
// endpoint was measured to accept, and to a mnemonic that never lives in git.
func TestModeratoExampleConfig(t *testing.T) {
	t.Setenv("TEMPO_BENCH_MNEMONIC", "from the environment")
	cfg, err := Load("../../examples/config.tempo.moderato.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	b := cfg.Benchmark
	if b.RemoteRPCURL != "https://rpc.moderato.tempo.xyz" || b.EVMChainID != 42431 {
		t.Errorf("endpoint/chain = %s/%d", b.RemoteRPCURL, b.EVMChainID)
	}
	if b.BaseMnemonic != "from the environment" {
		t.Errorf("base_mnemonic = %q, want it read from $TEMPO_BENCH_MNEMONIC", b.BaseMnemonic)
	}
	// The gateway answers a batch of 10 eth_sendRawTransaction with 413
	// "request too large"; every transaction in it would be lost.
	if b.BroadcastBatchSize < 1 || b.BroadcastBatchSize > 5 {
		t.Errorf("broadcast_batch_size = %d, want 1..5 for this endpoint", b.BroadcastBatchSize)
	}
	if !b.TempoFaucet || b.StartNode {
		t.Errorf("tempo_faucet=%v start_node=%v, want a faucet-funded run that starts nothing", b.TempoFaucet, b.StartNode)
	}
}

func TestRemoteRPCURLValidation(t *testing.T) {
	base := `benchmark:
  data_dir: /tmp/x
  chain_family: tempo
  runner_type: local
  remote_rpc_url: https://rpc.example.xyz
  erc20_transfer_gas: 300000
`
	for _, tc := range []struct {
		name  string
		edit  func(string) string
		wants string
	}{
		{"ok", func(s string) string { return s }, ""},
		{"cosmos family", func(s string) string { return strings.Replace(s, "chain_family: tempo", "chain_family: cosmos", 1) }, "tempo only"},
		{"starts a node", func(s string) string { return s + "  start_node: true\n" }, "start_node: false"},
		{"docker runner", func(s string) string { return strings.Replace(s, "runner_type: local", "runner_type: docker", 1) }, "runner_type: local"},
		{"two senders", func(s string) string { return s + "  validators: 2\n" }, "validators: 1"},
		{"not a URL", func(s string) string {
			return strings.Replace(s, "https://rpc.example.xyz", "rpc.example.xyz", 1)
		}, "http(s) URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(path, []byte(tc.edit(base)), 0o644); err != nil {
				t.Fatal(err)
			}
			// The generate path funds accounts, so it must refuse these too.
			for _, load := range []func(string) (AppConfig, error){Load, LoadForGenerate} {
				_, err := load(path)
				switch {
				case tc.wants == "" && err != nil:
					t.Errorf("unexpected error: %v", err)
				case tc.wants != "" && (err == nil || !strings.Contains(err.Error(), tc.wants)):
					t.Errorf("err = %v, want it to mention %q", err, tc.wants)
				}
			}
		})
	}
}
