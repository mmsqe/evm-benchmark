package activities

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// TestPortBlockDefaultBase covers the one branch the per-family port tests do
// not: both pass an explicit base, so nothing else pins what happens when the
// spec leaves it unset.
func TestPortBlockDefaultBase(t *testing.T) {
	block := portBlock{defaultBase: 9000, size: 4, rpcOffset: 3}
	if got := block.rpc(0, 2); got != 9011 {
		t.Errorf("rpc(unset, 2) = %d, want 9011 (default base + 2 blocks + offset)", got)
	}
}

// TestDevnetLaunchCommand pins that the launcher is looked up where the
// generators write it; the gated bootstrap tests are the only other check, and
// they skip without the chain binaries.
func TestDevnetLaunchCommand(t *testing.T) {
	spec := messages.BenchmarkSpec{DataDir: "/tmp/bench"}
	argv, dir := devnetLaunchCommand(spec, messages.NodeTarget{GlobalSeq: 2})
	if len(argv) != 1 || argv[0] != "/tmp/bench/devnet/node2/run.sh" || dir != "/tmp/bench/devnet/node2" {
		t.Errorf("devnetLaunchCommand = %v, %q", argv, dir)
	}
}

func TestPatchGenesisJSONPreservesLargeInts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "genesis.json")
	orig := `{"alloc":{"0xabc":{"balance":18446744073709551615}},"config":{"chainId":1337,"keep":true}}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := map[string]interface{}{
		"config": map[string]interface{}{"chainId": 9999},
	}
	if err := patchGenesisJSON(path, patch); err != nil {
		t.Fatalf("patchGenesisJSON: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "18446744073709551615") {
		t.Errorf("large balance corrupted (scientific notation?):\n%s", got)
	}
	if !strings.Contains(got, "9999") || !strings.Contains(got, `"keep": true`) {
		t.Errorf("patch merge wrong (want chainId 9999, keep true):\n%s", got)
	}
}

// TestDeepMergeJSON guards the genesis-patch semantics: nested objects merge
// recursively while scalars and arrays are replaced wholesale.
func TestDeepMergeJSON(t *testing.T) {
	dst := map[string]interface{}{
		"config": map[string]interface{}{"chainId": float64(1), "keep": "me"},
		"scalar": "old",
	}
	src := map[string]interface{}{
		"config": map[string]interface{}{"chainId": float64(2)},
		"scalar": "new",
	}
	out := deepMergeJSON(dst, src)
	cfg := out["config"].(map[string]interface{})
	if cfg["chainId"] != float64(2) {
		t.Errorf("chainId not overridden: %v", cfg["chainId"])
	}
	if cfg["keep"] != "me" {
		t.Errorf("sibling key dropped during merge: %v", cfg)
	}
	if out["scalar"] != "new" {
		t.Errorf("scalar not replaced: %v", out["scalar"])
	}
}
