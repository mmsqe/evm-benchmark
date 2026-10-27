package activities

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// TestAllegroRunScriptFormat pins the launcher: the flags are the contract with
// `allegro node`, and RUST_LOG must stay overridable from the environment.
func TestAllegroRunScriptFormat(t *testing.T) {
	spec := messages.BenchmarkSpec{
		AllegroBasePort:        9000,
		AllegroLeaderTimeoutMS: 1000,
		AllegroCertTimeoutMS:   2000,
		AllegroGasLimit:        1_000_000_000,
		NumAccounts:            500,
		NumTxs:                 40,
	}
	assertScript(t, allegroRunScript("/opt/allegro", allegroNodeArgs(spec, 1, allegroBasePort(spec, 1))), runScript{
		Binary: "/opt/allegro",
		Args: strings.Fields(`node --chain ./genesis.json --datadir . --http --http.addr 127.0.0.1 --http.port 9007
			--http.api all --authrpc.port 9006 --port 9005 --disable-discovery --ipcdisable
			--consensus.node-index 1 --consensus.listen-address 127.0.0.1:9004
			--consensus.leader-timeout 1000 --consensus.cert-timeout 2000 --builder.gaslimit 1000000000
			--txpool.max-account-slots 40 --txpool.pending-max-count 20000 --txpool.pending-max-size 20
			--txpool.queued-max-count 20000 --txpool.queued-max-size 20`),
		Preamble: []string{`: "${RUST_LOG:=allegro=info,commonware=warn}"`, "export RUST_LOG"},
	})
}

// TestAllegroNodeArgsOmitUnsetKnobs guards against silently changing what is
// measured: with no timeouts or gas limit configured the launcher must not
// invent values, leaving the binary's own defaults in place.
func TestAllegroNodeArgsOmitUnsetKnobs(t *testing.T) {
	args := allegroNodeArgs(messages.BenchmarkSpec{}, 0, defaultAllegroBasePort)
	for _, name := range []string{"--consensus.leader-timeout", "--consensus.cert-timeout", "--builder.gaslimit"} {
		if slices.Contains(args, name) {
			t.Errorf("%s passed although the spec leaves it unset: %v", name, args)
		}
	}
}

// TestAllegroNodeArgsAppendsExtra pins allegro_node_args: extra flags are
// appended verbatim last, so an operator can override any default above.
func TestAllegroNodeArgsAppendsExtra(t *testing.T) {
	extra := []string{"--builder.interval", "200ms"}
	args := allegroNodeArgs(messages.BenchmarkSpec{AllegroNodeArgs: extra}, 0, defaultAllegroBasePort)
	if n := len(args); args[n-2] != extra[0] || args[n-1] != extra[1] {
		t.Errorf("extra args not appended last: %v", args[len(args)-4:])
	}
}

// TestAllegroValidatorSockets pins the port-block scheme: the consensus socket
// baked into genesis must be the same one the launcher listens on, or peers
// dial a port nothing is bound to.
func TestAllegroValidatorSockets(t *testing.T) {
	spec := messages.BenchmarkSpec{AllegroBasePort: 9000}
	nodes := []messages.NodeTarget{{GlobalSeq: 0}, {GlobalSeq: 1}, {GlobalSeq: 2}}
	got := strings.Join(allegroValidatorSockets(spec, nodes), ",")
	if want := "127.0.0.1:9000,127.0.0.1:9004,127.0.0.1:9008"; got != want {
		t.Errorf("sockets = %q, want %q", got, want)
	}
	for _, node := range nodes {
		base := allegroBasePort(spec, node.GlobalSeq)
		args := strings.Join(allegroNodeArgs(spec, node.GlobalSeq, base), " ")
		socket := allegroValidatorSockets(spec, []messages.NodeTarget{node})[0]
		if !strings.Contains(args, "--consensus.listen-address "+socket) {
			t.Errorf("node %d listens somewhere other than its genesis socket %s: %s", node.GlobalSeq, socket, args)
		}
	}
	if got, want := (allegroRuntime{}).EVMRPCPort(spec, 1), 9007; got != want {
		t.Errorf("EVMRPCPort(node 1) = %d, want %d", got, want)
	}
}

// TestAllegroFundedAllocMatchesSigner is the load-bearing check: allegro-xtask
// funds 20 accounts on HD branch 0 only, so every account the generator signs
// from must be added to the alloc here. An address the signer uses but the
// alloc misses is a transaction that can never pay.
func TestAllegroFundedAllocMatchesSigner(t *testing.T) {
	spec := messages.BenchmarkSpec{
		NumAccounts:       3,
		NumTxs:            1,
		EVMChainID:        1337,
		GasPriceWei:       1_000_000_000,
		SimpleTransferGas: 21_000,
		TxType:            messages.SimpleTransferTx,
		BaseMnemonic:      publicTestMnemonic,
	}
	nodes := []messages.NodeTarget{{GlobalSeq: 0}, {GlobalSeq: 1}}
	alloc, err := allegroFundedAlloc(spec, nodes)
	if err != nil {
		t.Fatalf("allegroFundedAlloc: %v", err)
	}
	if len(alloc) != len(nodes)*spec.NumAccounts {
		t.Fatalf("funded %d accounts, want %d", len(alloc), len(nodes)*spec.NumAccounts)
	}
	for _, node := range nodes {
		for _, sender := range signedSenders(t, spec, node.GlobalSeq) {
			if _, ok := alloc[allocKey(sender)]; !ok {
				t.Errorf("node %d signs from unfunded account %s", node.GlobalSeq, sender.Hex())
			}
		}
	}

	// Index 0 is never used for load, so funding it would be dead weight.
	key, err := keygen.DeterministicKey(0, 0, spec.BaseMnemonic)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := alloc[allocKey(crypto.PubkeyToAddress(key.PublicKey))]; ok {
		t.Error("index 0 funded although no transaction is signed with it")
	}
}

// TestAllegroGenesisPatchShape pins the merge order and encodings: gasLimit has
// to be a hex quantity (alloy would read a decimal as a far smaller budget) and
// the operator's genesis_patch must win over the family defaults.
func TestAllegroGenesisPatchShape(t *testing.T) {
	spec := messages.BenchmarkSpec{NumAccounts: 1, BaseMnemonic: publicTestMnemonic, AllegroGasLimit: 1_000_000_000}
	nodes := []messages.NodeTarget{{GlobalSeq: 0}}
	patch, err := allegroGenesisPatch(spec, nodes)
	if err != nil {
		t.Fatalf("allegroGenesisPatch: %v", err)
	}
	if got := patch["gasLimit"]; got != "0x3b9aca00" {
		t.Errorf("gasLimit = %v, want the hex quantity 0x3b9aca00", got)
	}
	if _, ok := patch["alloc"].(map[string]interface{}); !ok {
		t.Errorf("alloc missing from patch: %v", patch)
	}

	spec.GenesisPatch = map[string]interface{}{"gasLimit": "0x1"}
	if patch, err = allegroGenesisPatch(spec, nodes); err != nil {
		t.Fatalf("allegroGenesisPatch: %v", err)
	}
	if got := patch["gasLimit"]; got != "0x1" {
		t.Errorf("genesis_patch must override the family gas limit, got %v", got)
	}
}

// TestAllegroBootstrap exercises the real generation path (which shells out to
// `allegro-xtask genesis`). It is skipped unless the toolchain is available:
//
//	ALLEGRO_XTASK_BIN=.../allegro-xtask go test ./internal/activities -run AllegroBootstrap
func TestAllegroBootstrap(t *testing.T) {
	xtaskBin := os.Getenv("ALLEGRO_XTASK_BIN")
	if xtaskBin == "" {
		t.Skip("set ALLEGRO_XTASK_BIN to run")
	}

	spec := messages.BenchmarkSpec{
		ChainFamily:     FamilyAllegro,
		DataDir:         t.TempDir(),
		EVMChainID:      1337,
		NumAccounts:     8,
		NumTxs:          4,
		BaseMnemonic:    publicTestMnemonic,
		Validators:      2,
		AllegroXtaskBin: xtaskBin,
		AllegroBasePort: freePortBlock(t),
		AllegroGasLimit: 1_000_000_000,
	}
	nodes := []messages.NodeTarget{
		{GlobalSeq: 0, Group: "validators", GroupSeq: 0},
		{GlobalSeq: 1, Group: "validators", GroupSeq: 1},
	}
	if err := (allegroRuntime{}).Bootstrap(context.Background(), spec, nodes); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	var genesis struct {
		Alloc    map[string]struct{ Balance string } `json:"alloc"`
		GasLimit string                              `json:"gasLimit"`
		Config   struct {
			ChainID int64 `json:"chainId"`
		} `json:"config"`
		Validators []struct {
			Ingress string `json:"ingress"`
		} `json:"validators"`
	}
	if err := readJSON(filepath.Join(devnetDir(spec), "genesis.json"), &genesis); err != nil {
		t.Fatalf("read genesis: %v", err)
	}
	if genesis.Config.ChainID != spec.EVMChainID {
		t.Errorf("chainId = %d, want %d", genesis.Config.ChainID, spec.EVMChainID)
	}
	if genesis.GasLimit != "0x3b9aca00" {
		t.Errorf("gasLimit = %q, want 0x3b9aca00", genesis.GasLimit)
	}
	sockets := allegroValidatorSockets(spec, nodes)
	if len(genesis.Validators) != len(sockets) {
		t.Fatalf("genesis has %d validators, want %d", len(genesis.Validators), len(sockets))
	}
	for i, v := range genesis.Validators {
		if v.Ingress != sockets[i] {
			t.Errorf("validator %d ingress = %q, want %q", i, v.Ingress, sockets[i])
		}
	}
	// Every signing account of every node must be funded, on top of the 20
	// accounts allegro-xtask prefunds itself.
	signers, err := signerAddresses(spec, nodes)
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range signers {
		if got, ok := genesis.Alloc[allocKey(addr)]; !ok || got.Balance == "" {
			t.Errorf("signer %s is not funded in genesis", addr.Hex())
		}
	}

	for _, node := range nodes {
		home := devnetNodeHome(spec, node.GlobalSeq)
		for _, name := range []string{"run.sh", "genesis.json"} {
			if _, err := os.Stat(filepath.Join(home, name)); err != nil {
				t.Errorf("node %d is missing %s: %v", node.GlobalSeq, name, err)
			}
		}
	}
}

// freePortBlock returns a base port whose block is (very likely) unused, so a
// bootstrap test does not trip the stale-node guard on a busy machine.
func freePortBlock(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

// TestAllegroValidateRejectsUnsupported pins the configurations that would
// otherwise be mis-run: there is no allegro docker image, no non-validating
// node type, and no node to start without a binary.
func TestAllegroValidateRejectsUnsupported(t *testing.T) {
	for name, spec := range map[string]messages.BenchmarkSpec{
		"docker":    {RunnerType: "docker"},
		"fullnodes": {RunnerType: "local", Fullnodes: 1},
		"no binary": {RunnerType: "local", StartNode: true},
	} {
		if err := (allegroRuntime{}).Validate(spec); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	ok := messages.BenchmarkSpec{RunnerType: "local", StartNode: true, AllegroBin: "allegro"}
	if err := (allegroRuntime{}).Validate(ok); err != nil {
		t.Errorf("valid local spec rejected: %v", err)
	}
}

// TestAllegroRejectsUnbackedERC20 guards a silent-wrongness path: an ERC-20
// transfer to an address with no code succeeds and burns 21k gas, so it would
// report throughput for a workload that never ran a contract.
func TestAllegroRejectsUnbackedERC20(t *testing.T) {
	spec := messages.BenchmarkSpec{TxType: messages.ERC20TransferTx}
	if err := (allegroRuntime{}).EnrichSpec(&spec); err == nil {
		t.Fatal("expected erc20-transfer without a contract address to be rejected")
	}

	withContract := messages.BenchmarkSpec{
		TxType:               messages.ERC20TransferTx,
		ERC20ContractAddress: "0x1000000000000000000000000000000000000000",
	}
	if err := (allegroRuntime{}).EnrichSpec(&withContract); err != nil {
		t.Errorf("erc20-transfer with an explicit contract rejected: %v", err)
	}

	empty := messages.BenchmarkSpec{}
	if err := (allegroRuntime{}).EnrichSpec(&empty); err != nil {
		t.Fatalf("EnrichSpec: %v", err)
	}
	if empty.TxType != messages.SimpleTransferTx {
		t.Errorf("tx_type = %q, want the native transfer default", empty.TxType)
	}
}
