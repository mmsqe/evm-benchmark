package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandPath pins the ~ / $VAR expansion used for config paths.
func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home dir: %v", err)
	}
	t.Setenv("EBTEST_DIR", "/opt/eb")

	cases := map[string]string{
		"":                "",
		"/abs/path":       "/abs/path",
		"~":               home,
		"~/tempo/data":    filepath.Join(home, "tempo/data"),
		"$EBTEST_DIR/x":   "/opt/eb/x",
		"${EBTEST_DIR}/y": "/opt/eb/y",
		"~notme/keep-lit": "~notme/keep-lit", // only leading ~/ (or bare ~) expands
	}
	for in, want := range cases {
		if got := expandPath(in); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDeepMerge guards the patch semantics: nested objects merge recursively,
// scalars are replaced, and an object src adds stays src's own.
func TestDeepMerge(t *testing.T) {
	dst := map[string]any{
		"config": map[string]any{"chainId": float64(1), "keep": "me"},
		"scalar": "old",
	}
	added := map[string]any{"x": 1}
	src := map[string]any{
		"config": map[string]any{"chainId": float64(2)},
		"scalar": "new",
		"added":  added,
	}
	out := DeepMerge(dst, src)
	cfg := out["config"].(map[string]any)
	if cfg["chainId"] != float64(2) || cfg["keep"] != "me" {
		t.Errorf("config = %v, want chainId 2 beside keep", cfg)
	}
	if out["scalar"] != "new" {
		t.Errorf("scalar not replaced: %v", out["scalar"])
	}
	out["added"].(map[string]any)["x"] = 2
	if added["x"] != 1 {
		t.Error("merging into the result wrote through to the patch")
	}
}

func TestEvaluateChainProfile(t *testing.T) {
	chains, err := filepath.Abs("../../config/chains.jsonnet")
	if err != nil {
		t.Fatal(err)
	}
	out, err := evaluateChainProfile(chains, "evmd")
	if err != nil {
		t.Fatalf("evmd: %v", err)
	}
	var record chainConfigRecord
	if err := json.Unmarshal(out, &record); err != nil || record.Cmd != "evmd" || record.EVMChainID != 262144 {
		t.Errorf("evmd profile = %+v (%v), want cmd evmd on chain 262144", record, err)
	}
	if _, err := evaluateChainProfile(chains, "nope"); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("err = %v, want the unknown profile named", err)
	}
}
