package activities

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// Plumbing shared by the families that generate their own network under
// <data_dir>/devnet rather than driving a chain binary's init/genesis commands
// (tempo, allegro, reth). They differ in what goes into a node home and how
// peers are wired; the layout, port block, genesis patch, signer funding and
// launcher format are the same.

// devnetRuntime is the ChainRuntime behaviour every such family shares; each
// embeds it and adds what differs.
type devnetRuntime struct{}

// HasConsensusRPC reports false: these nodes expose no CometBFT-style RPC, so
// readiness is determined from the EVM JSON-RPC port alone.
func (devnetRuntime) HasConsensusRPC() bool { return false }

// LocalStartCommand runs the launcher generated for the node.
func (devnetRuntime) LocalStartCommand(spec messages.BenchmarkSpec, target messages.NodeTarget) ([]string, string) {
	return devnetLaunchCommand(spec, target)
}

// rpcPorter is the part of ChainRuntime the port checks below need.
type rpcPorter interface {
	EVMRPCPort(spec messages.BenchmarkSpec, globalSeq int) int
}

// devnetPreStartCheck refuses to launch when the node's RPC port is already
// served, which would otherwise mean benchmarking a leftover node's stale
// genesis. An externally managed node, including a compose-managed container,
// is expected to be listening already.
func devnetPreStartCheck(chain rpcPorter, spec messages.BenchmarkSpec, target messages.NodeTarget) error {
	if !spec.StartNode || spec.RunnerType == "docker" {
		return nil
	}
	return assertRPCPortsFree(chain, spec, []messages.NodeTarget{target})
}

// devnetDir is the root of a generated network.
func devnetDir(spec messages.BenchmarkSpec) string {
	return filepath.Join(spec.DataDir, "devnet")
}

// resetDevnetDir clears and recreates the devnet root, returning it. Bootstrap
// runs more than once per benchmark (benchctl gen, then the GenerateLayout
// activity), so regenerating has to be idempotent.
func resetDevnetDir(spec messages.BenchmarkSpec) (string, error) {
	dir := devnetDir(spec)
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("clear devnet dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create devnet dir: %w", err)
	}
	return dir, nil
}

// nodeMoniker is a node's directory name (and docker service name).
func nodeMoniker(globalSeq int) string { return fmt.Sprintf("node%d", globalSeq) }

// devnetNodeHome is the per-node directory inside the generated network.
func devnetNodeHome(spec messages.BenchmarkSpec, globalSeq int) string {
	return filepath.Join(devnetDir(spec), nodeMoniker(globalSeq))
}

// devnetLaunchCommand runs the node's generated launcher, which resolves its own
// relative paths and so must run from the node home.
func devnetLaunchCommand(spec messages.BenchmarkSpec, target messages.NodeTarget) ([]string, string) {
	home := devnetNodeHome(spec, target.GlobalSeq)
	return []string{filepath.Join(home, "run.sh")}, home
}

// writeDevnetNodes writes each node's home with the shared genesis and the
// launcher returned for it. The home doubles as the node's datadir.
func writeDevnetNodes(
	spec messages.BenchmarkSpec,
	nodes []messages.NodeTarget,
	genesis []byte,
	launcher func(messages.NodeTarget) (runScript, error),
) error {
	for _, node := range nodes {
		nodeDir := devnetNodeHome(spec, node.GlobalSeq)
		if err := os.MkdirAll(nodeDir, 0o755); err != nil {
			return fmt.Errorf("create node dir %s: %w", nodeDir, err)
		}
		if err := os.WriteFile(filepath.Join(nodeDir, "genesis.json"), genesis, 0o644); err != nil {
			return fmt.Errorf("write genesis to %s: %w", nodeDir, err)
		}
		script, err := launcher(node)
		if err != nil {
			return err
		}
		if err := script.write(nodeDir); err != nil {
			return err
		}
	}
	return nil
}

// devnetAccountBalance funds each signer account with 10,000 ETH, as
// allegro-xtask funds its own accounts, in the hex quantity alloc entries use.
const devnetAccountBalance = "0x21e19e0c9bab2400000"

// signerAddresses returns, for every node, the addresses the shared signer
// sends from: bench.signAccountTxs signs account i with key index i+1, so index
// 0 is never used for load. An address funded from this list is one that can
// pay.
func signerAddresses(spec messages.BenchmarkSpec, nodes []messages.NodeTarget) ([]common.Address, error) {
	addrs := make([]common.Address, 0, len(nodes)*spec.NumAccounts)
	for _, node := range nodes {
		for i := 1; i <= spec.NumAccounts; i++ {
			key, err := keygen.DeterministicKey(node.GlobalSeq, i, spec.BaseMnemonic)
			if err != nil {
				return nil, fmt.Errorf("derive account %d for node %d: %w", i, node.GlobalSeq, err)
			}
			addrs = append(addrs, crypto.PubkeyToAddress(key.PublicKey))
		}
	}
	return addrs, nil
}

// allocKey spells an address the way genesis generators write alloc entries,
// lowercase, so an account one already funded merges into a single entry.
func allocKey(addr common.Address) string {
	return strings.ToLower(addr.Hex())
}

// portBlock is a family's fixed per-node port allocation: node N's ports start
// at base + N*size, with EVM JSON-RPC at rpcOffset inside the block. Declaring
// one per family keeps "where does node 2 serve JSON-RPC" to a single answer.
type portBlock struct {
	defaultBase int
	size        int
	rpcOffset   int
}

// base returns node globalSeq's first port; specBase 0 means unset, so the
// family default applies.
func (b portBlock) base(specBase, globalSeq int) int {
	if specBase == 0 {
		specBase = b.defaultBase
	}
	return specBase + globalSeq*b.size
}

func (b portBlock) rpc(specBase, globalSeq int) int {
	return b.base(specBase, globalSeq) + b.rpcOffset
}

// assertRPCPortsFree refuses to continue when any node's JSON-RPC port is
// already served: a node left over from an earlier run keeps serving its own
// (stale) genesis, and the load would silently measure that chain instead of
// the one generated here.
func assertRPCPortsFree(chain rpcPorter, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	for _, node := range nodes {
		if err := assertPortFree(chain.EVMRPCPort(spec, node.GlobalSeq)); err != nil {
			return err
		}
	}
	return nil
}

func assertPortFree(port int) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err != nil {
		return nil // nothing listening: the port is ours to use
	}
	_ = conn.Close()
	return fmt.Errorf(
		"port %d is already in use: a node from a previous run is still serving its own genesis; "+
			"stop it first (scripts/run-benchmark.sh --mode <mode> stop)", port)
}

// reth's transaction pool defaults hold ~5,000 transactions: 16 executable
// slots per sender and 10,000 per sub-pool. A benchmark load overruns them and
// the excess is refused at submission, so a run reports a fraction of its load
// included and a rate that measures nothing. Both reth-based families size the
// pool from the spec instead of a hardcoded number, so raising num_txs cannot
// silently leave it stale.
const (
	poolMinCount   = 10_000
	poolMinSizeMB  = 20
	poolMinSlots   = 16
	poolBytesPerTx = 512
	bytesPerMiB    = 1 << 20
)

// poolCapacity is how much pool a run needs: room for every transaction, and
// per-sender slots covering num_txs, since the generator signs that many
// sequential nonces per account and a sender past its limit is rejected rather
// than queued. Both the reth flags and evmd's app-side mempool derive from it.
func poolCapacity(spec messages.BenchmarkSpec) (count, slots int) {
	total := max(spec.NumAccounts, 0) * max(spec.NumTxs, 0)
	return max(total, poolMinCount), max(spec.NumTxs, poolMinSlots)
}

// rethTxPoolArgs renders poolCapacity as reth's --txpool.* flags.
func rethTxPoolArgs(spec messages.BenchmarkSpec) []string {
	count, slots := poolCapacity(spec)
	sizeMB := max(count*poolBytesPerTx/bytesPerMiB+1, poolMinSizeMB)
	return []string{
		"--txpool.max-account-slots", strconv.Itoa(slots),
		"--txpool.pending-max-count", strconv.Itoa(count),
		"--txpool.pending-max-size", strconv.Itoa(sizeMB),
		"--txpool.queued-max-count", strconv.Itoa(count),
		"--txpool.queued-max-size", strconv.Itoa(sizeMB),
	}
}

// runScript is the launcher written beside each node: an `exec`'d command with
// the binary and every argument single-quoted on its own continued line. The
// exact shape is what the node binaries were verified against, so it is pinned
// byte-for-byte by tests.
type runScript struct {
	Binary string
	Args   []string // argv after the binary
	Note   string   // appended to the "auto-generated" header, e.g. " (docker)"
	Strict bool     // emit `set -eu`
	// Preamble lines go between the cd and the exec, e.g. a default RUST_LOG
	// the caller's environment can still override.
	Preamble []string
}

func (s runScript) render() string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("# auto-generated by evm-benchmark" + s.Note + "\n\n")
	if s.Strict {
		b.WriteString("set -eu\n\n")
	}
	b.WriteString("cd \"$(dirname \"$0\")\"\n\n")
	if len(s.Preamble) > 0 {
		b.WriteString(strings.Join(s.Preamble, "\n") + "\n\n")
	}
	b.WriteString("exec " + shQuote(s.Binary) + " \\\n")
	for i, a := range s.Args {
		b.WriteString("  " + shQuote(a))
		if i < len(s.Args)-1 {
			b.WriteString(" \\")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// write renders the launcher into the node home.
func (s runScript) write(nodeHome string) error {
	return writeExecutable(filepath.Join(nodeHome, "run.sh"), s.render())
}

func writeExecutable(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// shQuote wraps a string in single quotes, escaping embedded single quotes.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// patchGenesisJSON deep-merges patch into the genesis file: objects merge
// recursively, everything else is replaced.
func patchGenesisJSON(genesisPath string, patch map[string]interface{}) error {
	raw, err := os.ReadFile(genesisPath)
	if err != nil {
		return fmt.Errorf("read genesis for patch: %w", err)
	}
	// Decode with UseNumber so integers beyond float64's exact range (token
	// balances reach 2**64-1) survive the round-trip verbatim instead of being
	// mangled into scientific notation.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]interface{}
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("decode genesis: %w", err)
	}
	merged := deepMergeJSON(doc, patch)
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return fmt.Errorf("encode patched genesis: %w", err)
	}
	if err := os.WriteFile(genesisPath, out, 0o644); err != nil {
		return fmt.Errorf("write patched genesis: %w", err)
	}
	return nil
}

// deepMergeJSON recursively merges src into dst for nested objects; any
// non-object value in src replaces the corresponding value in dst.
func deepMergeJSON(dst, src map[string]interface{}) map[string]interface{} {
	for k, sv := range src {
		if svMap, ok := sv.(map[string]interface{}); ok {
			if dvMap, ok := dst[k].(map[string]interface{}); ok {
				dst[k] = deepMergeJSON(dvMap, svMap)
				continue
			}
		}
		dst[k] = sv
	}
	return dst
}
