package activities

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// This file generates an Allegro devnet in-process. `allegro-xtask genesis` (a
// Rust binary in the allegro repo) writes a genesis.json with the validator set
// (public keys and consensus sockets) embedded; everything it does not do —
// funding the benchmark's own accounts, sizing the block gas budget and writing
// a per-node launcher — is done here.
//
// allegro-xtask funds exactly 20 accounts from the anvil mnemonic on HD branch
// 0, while the benchmark signs from branch <node index> at indices
// 1..num_accounts, so the alloc patch below is what makes any load land: without
// it every transaction beyond the first 19 of node 0 comes from an account with
// a zero balance.

// generateAllegroDevnet materialises an Allegro devnet under spec.DataDir/devnet:
// a shared genesis.json plus one directory per node holding its own copy of the
// genesis and a `run.sh` launcher. Each node directory doubles as its reth
// datadir.
func generateAllegroDevnet(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	dataDir, err := resetDevnetDir(spec)
	if err != nil {
		return err
	}

	xtaskBin := cmp.Or(spec.AllegroXtaskBin, allegroDefaultXtaskBin)
	sockets := allegroValidatorSockets(spec, nodes)
	if _, err := chainCmd(ctx, xtaskBin, nil,
		allegroXtaskGenesisArgs(spec, dataDir, strings.Join(sockets, ","))...); err != nil {
		return fmt.Errorf("allegro-xtask genesis: %w", err)
	}

	patch, err := allegroGenesisPatch(spec, nodes)
	if err != nil {
		return err
	}
	genesisPath := filepath.Join(dataDir, "genesis.json")
	if err := patchGenesisJSON(genesisPath, patch); err != nil {
		return err
	}
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return fmt.Errorf("read genesis: %w", err)
	}

	nodeBin := cmp.Or(spec.AllegroBin, allegroDefaultNodeBin)
	return writeDevnetNodes(spec, nodes, genesisBytes, func(node messages.NodeTarget) (runScript, error) {
		args := allegroNodeArgs(spec, node.GlobalSeq, allegroBasePort(spec, node.GlobalSeq))
		return allegroRunScript(nodeBin, args), nil
	})
}

// allegroValidatorSockets lists each node's consensus p2p socket, in node-index
// order. allegro-xtask bakes these into the genesis validator set, and every
// node dials its peers there, so they must match the --consensus.listen-address
// the launcher passes.
func allegroValidatorSockets(spec messages.BenchmarkSpec, nodes []messages.NodeTarget) []string {
	sockets := make([]string, 0, len(nodes))
	for _, node := range nodes {
		sockets = append(sockets, fmt.Sprintf("127.0.0.1:%d", allegroBasePort(spec, node.GlobalSeq)))
	}
	return sockets
}

// allegroXtaskGenesisArgs builds the `allegro-xtask genesis` invocation. The
// validator sockets are passed explicitly rather than as a count, so the
// generated set matches this run's port blocks.
func allegroXtaskGenesisArgs(spec messages.BenchmarkSpec, outputDir, validatorsArg string) []string {
	return []string{
		"genesis",
		"--validators", validatorsArg,
		"--chain-id", strconv.FormatInt(spec.EVMChainID, 10),
		"--output", outputDir,
	}
}

// allegroGenesisPatch is the deep merge applied to the generated genesis: the
// benchmark's funded accounts, the block gas budget, and finally the operator's
// own genesis_patch, which wins over both.
func allegroGenesisPatch(spec messages.BenchmarkSpec, nodes []messages.NodeTarget) (map[string]interface{}, error) {
	alloc, err := allegroFundedAlloc(spec, nodes)
	if err != nil {
		return nil, err
	}
	patch := map[string]interface{}{"alloc": alloc}
	if spec.AllegroGasLimit > 0 {
		// alloy serialises gasLimit as a hex quantity; a decimal number here
		// would be read as a much smaller budget.
		patch["gasLimit"] = fmt.Sprintf("0x%x", spec.AllegroGasLimit)
	}
	return deepMergeJSON(patch, spec.GenesisPatch), nil
}

// allegroFundedAlloc is the genesis alloc entry for every account the load
// signs from.
func allegroFundedAlloc(spec messages.BenchmarkSpec, nodes []messages.NodeTarget) (map[string]interface{}, error) {
	addrs, err := signerAddresses(spec, nodes)
	if err != nil {
		return nil, err
	}
	alloc := make(map[string]interface{}, len(addrs))
	for _, addr := range addrs {
		alloc[allocKey(addr)] = map[string]interface{}{"balance": devnetAccountBalance}
	}
	return alloc, nil
}

// allegroNodeArgs builds the `allegro node` argument list (everything after the
// binary). Paths are node-dir-relative; the launcher cd's there first.
func allegroNodeArgs(spec messages.BenchmarkSpec, globalSeq, base int) []string {
	args := []string{
		"node",
		"--chain", "./genesis.json",
		"--datadir", ".",
		"--http",
		"--http.addr", "127.0.0.1",
		"--http.port", strconv.Itoa(base + allegroHTTPPortOffset),
		"--http.api", "all",
		"--authrpc.port", strconv.Itoa(base + allegroAuthRPCPortOffset),
		"--port", strconv.Itoa(base + allegroExecP2PPortOffset),
		// The devnet is fully described by genesis; execution peers are not
		// discovered, and an IPC socket would collide between node dirs.
		"--disable-discovery",
		"--ipcdisable",
		"--consensus.node-index", strconv.Itoa(globalSeq),
		"--consensus.listen-address", fmt.Sprintf("127.0.0.1:%d", base),
	}
	// Consensus timeouts are left at the binary's own defaults unless the spec
	// sets them: the block cadence they control is part of what is measured.
	if spec.AllegroLeaderTimeoutMS > 0 {
		args = append(args, "--consensus.leader-timeout", strconv.Itoa(spec.AllegroLeaderTimeoutMS))
	}
	if spec.AllegroCertTimeoutMS > 0 {
		args = append(args, "--consensus.cert-timeout", strconv.Itoa(spec.AllegroCertTimeoutMS))
	}
	if spec.AllegroGasLimit > 0 {
		// Pin the builder target too: reth otherwise carries the parent's gas
		// limit forward, and the genesis value alone can drift under the
		// EIP-1559 adjustment.
		args = append(args, "--builder.gaslimit", strconv.FormatInt(spec.AllegroGasLimit, 10))
	}
	args = append(args, rethTxPoolArgs(spec)...)
	// Extra flags last, so an operator can override any default above.
	return append(args, spec.AllegroNodeArgs...)
}

// allegroRunScript describes the node's `run.sh`: the launcher plus a default
// RUST_LOG the caller's environment can still override.
func allegroRunScript(binary string, nodeArgs []string) runScript {
	return runScript{
		Binary:   binary,
		Args:     nodeArgs,
		Preamble: []string{`: "${RUST_LOG:=allegro=info,commonware=warn}"`, "export RUST_LOG"},
	}
}
