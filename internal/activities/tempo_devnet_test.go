package activities

import (
	"slices"
	"strings"
	"testing"

	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// twoLocalVals / identities are a fixed 2-validator local fixture used to pin
// the generated launcher byte-for-byte.
var (
	twoLocalVals = []tempoDevnetValidator{
		{Moniker: "node0", Host: "127.0.0.1", Port: 8000},
		{Moniker: "node1", Host: "127.0.0.1", Port: 8006},
	}
	fixedIdentities = []string{"aaa", "bbb"}
)

// TestTempoRunScriptFormat pins the local and docker launchers. tempo-xtask
// emits keys and identities; the launcher wiring is ours, so it must match
// what `tempo node` was verified to accept.
func TestTempoRunScriptFormat(t *testing.T) {
	local := tempoNodeArgs(messages.BenchmarkSpec{}, 8006, "127.0.0.1", tempoLocalTrustedPeers(twoLocalVals, fixedIdentities), false)
	assertScript(t, tempoRunScript("/opt/tempo", local, false), runScript{
		Binary: "/opt/tempo",
		Args: strings.Fields(`node --consensus.signing-key ./signing.key --consensus.secret ./.secret
			--consensus.signing-share ./signing.share --consensus.listen-address 127.0.0.1:8006
			--consensus.metrics-address 127.0.0.1:8008 --chain ./genesis.json --datadir . --port 8007 --discovery.port 8007
			--p2p-secret-key ./enode.key --trusted-peers enode://aaa@127.0.0.1:8001,enode://bbb@127.0.0.1:8007
			--authrpc.port 8009 --http --http.addr 0.0.0.0 --http.port 8010 --http.api all
			--ws --ws.addr 0.0.0.0 --ws.port 8011 --consensus.use-local-defaults --consensus.allow-private-ips
			--txpool.max-account-slots 16 --txpool.pending-max-count 10000 --txpool.pending-max-size 20
			--txpool.queued-max-count 10000 --txpool.queued-max-size 20`),
	})

	docker := tempoNodeArgs(messages.BenchmarkSpec{}, tempoDockerConsensusPort, "0.0.0.0", tempoDockerTrustedPeers(twoLocalVals, fixedIdentities, 0), true)
	assertScript(t, tempoRunScript("tempo", docker, true), runScript{
		Binary: "tempo",
		Args: strings.Fields(`node --consensus.signing-key ./signing.key --consensus.secret ./.secret
			--consensus.signing-share ./signing.share --consensus.listen-address 0.0.0.0:8000
			--consensus.metrics-address 0.0.0.0:8002 --chain ./genesis.json --datadir . --port 8001 --discovery.port 8001
			--p2p-secret-key ./enode.key --trusted-peers enode://bbb@node1:8001
			--authrpc.port 8003 --http --http.addr 0.0.0.0 --http.port 8004 --http.api all
			--ws --ws.addr 0.0.0.0 --ws.port 8005 --consensus.use-local-defaults --consensus.allow-private-ips
			--tempo.bootnodes-endpoint none
			--txpool.max-account-slots 16 --txpool.pending-max-count 10000 --txpool.pending-max-size 20
			--txpool.queued-max-count 10000 --txpool.queued-max-size 20`),
		Note:   " (docker)",
		Strict: true,
	})
}

// TestTempoNodeArgsAppendsExtra pins tempo_node_args: extra flags are appended
// verbatim last, so they override any default, which is how an operator raises
// the per-block build budget via --consensus.target-block-time to probe the
// ~5,000-tx cap.
func TestTempoNodeArgsAppendsExtra(t *testing.T) {
	extra := []string{"--consensus.target-block-time", "2s"}
	spec := messages.BenchmarkSpec{TempoNodeArgs: extra}
	for _, docker := range []bool{false, true} {
		args := tempoNodeArgs(spec, 8000, "127.0.0.1", nil, docker)
		if tail := args[len(args)-len(extra):]; !slices.Equal(tail, extra) {
			t.Errorf("docker=%t: args end in %v, want the extra args %v last", docker, tail, extra)
		}
	}
}

// TestTempoTrustedPeers pins the peer-wiring divergence: local advertises every
// validator (self included) at its host loopback and execution-p2p port; docker
// advertises the OTHER validators by service name at the fixed 8001 port.
func TestTempoTrustedPeers(t *testing.T) {
	local := tempoLocalTrustedPeers(twoLocalVals, fixedIdentities)
	if want := []string{"enode://aaa@127.0.0.1:8001", "enode://bbb@127.0.0.1:8007"}; !slices.Equal(local, want) {
		t.Errorf("local peers = %v, want %v", local, want)
	}

	docker := tempoDockerTrustedPeers(twoLocalVals, fixedIdentities, 1)
	if want := []string{"enode://aaa@node0:8001"}; !slices.Equal(docker, want) {
		t.Errorf("docker peers (self=1) = %v, want %v", docker, want)
	}
}

// TestTempoDockerIP pins the static-IP scheme baked into genesis and compose:
// host octet base 10, one per validator, on the default subnet.
func TestTempoDockerIP(t *testing.T) {
	for idx, want := range map[int]string{0: "10.88.0.10", 1: "10.88.0.11", 3: "10.88.0.13"} {
		got, err := tempoDockerIP(tempoDockerSubnet, idx)
		if err != nil {
			t.Fatalf("index %d: %v", idx, err)
		}
		if got != want {
			t.Errorf("dockerIP(%d) = %q, want %q", idx, got, want)
		}
	}
}

// TestTempoXtaskGenesisArgs guards the genesis-arg defaults: an unset epoch/gas
// must fall back to 100 / 500M rather than 0, which tempo-xtask would reject or
// mis-size the block for.
func TestTempoXtaskGenesisArgs(t *testing.T) {
	base := messages.BenchmarkSpec{EVMChainID: 1337, NumAccounts: 8, Validators: 2, BaseMnemonic: publicTestMnemonic}
	want := []string{
		"--chain-id", "1337",
		"--accounts", "17", // 8*2 + 1 validator key
		"--epoch-length", "100",
		"--gas-limit", "500000000",
		"--mnemonic", publicTestMnemonic,
		"--validators", "127.0.0.1:8000,127.0.0.1:8006",
	}
	if got := tempoXtaskGenesisArgs(base, "127.0.0.1:8000,127.0.0.1:8006"); !slices.Equal(got, want) {
		t.Errorf("genesis args = %q, want %q", got, want)
	}

	overridden := base
	overridden.TempoEpochLength = 50
	overridden.TempoGasLimit = 3_000_000_000
	got := tempoXtaskGenesisArgs(overridden, "x")
	if flagValue(got, "--epoch-length") != "50" || flagValue(got, "--gas-limit") != "3000000000" {
		t.Errorf("overrides not honoured: %q", got)
	}
}
