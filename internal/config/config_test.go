package config

import (
	"os"
	"path/filepath"
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
