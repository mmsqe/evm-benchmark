package activities

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mmsqe/evm-benchmark/internal/messages"
	toml "github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
)

// This file generates a Tempo devnet in-process. `tempo-xtask generate-localnet`
// (a Rust binary in the tempo repo) produces genesis, consensus keys and enode
// identities; everything it does not do — renaming validator dirs to monikers,
// wiring trusted peers, writing per-node launchers and a docker-compose file —
// is done here, so the benchmark depends only on the two Rust binaries it
// already builds.
//
// Only the single-network subset the benchmark uses is reproduced: a local
// supervisor-free launch and a single-network docker compose (no two-network /
// follow-node / p2p-proxy topologies).

const (
	// tempoLocalnetSecret is the fixed passphrase tempo-xtask uses to encrypt
	// the localnet signing key; `tempo node` needs it back via --consensus.secret.
	// It must match generate_localnet.rs's LOCALNET_SIGNING_KEY_SECRET.
	tempoLocalnetSecret = "tempo-localnet-signing-key-secret"

	// Defaults applied when the spec leaves the corresponding field at its zero
	// value.
	tempoDefaultEpochLength = 100
	tempoDefaultGasLimit    = 500_000_000
	tempoDefaultXtaskBin    = "tempo-xtask"
	tempoDefaultNodeBin     = "tempo"

	// Docker single-network layout. Each container has its own netns, so every
	// validator reuses the same internal port block based at 8000. Consensus
	// peers are baked into genesis as numeric ip:port, so each container gets a
	// static IP on tempoDockerSubnet.
	tempoDockerConsensusPort   = 8000
	tempoDockerSubnet          = "10.88.0.0/24"
	tempoDockerIPHostOctetBase = 10
	tempoContainerDataDir      = "/data"
)

// tempoDevnetValidator is one validator in the generated network.
type tempoDevnetValidator struct {
	Moniker string // directory name and docker service name (node0, node1, ...)
	Host    string // advertised IP for local genesis + trusted peers
	Port    int    // consensus-p2p / base port for the node's 6-port block
}

// generateTempoDevnet materialises a Tempo devnet under spec.DataDir/devnet,
// producing the on-disk layout the benchmark launches from. In docker mode it
// also writes a single-network docker-compose.yaml; the caller starts the
// cluster with `docker compose up`.
func generateTempoDevnet(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	docker := spec.RunnerType == "docker"

	vals := make([]tempoDevnetValidator, 0, len(nodes))
	for _, node := range nodes {
		vals = append(vals, tempoDevnetValidator{
			Moniker: tempoMoniker(node.GlobalSeq),
			Host:    "127.0.0.1",
			Port:    tempoBasePort(spec, node.GlobalSeq),
		})
	}

	dataDir := filepath.Join(spec.DataDir, "devnet")
	// Clear the data dir before regenerating so re-bootstrap is idempotent.
	if err := os.RemoveAll(dataDir); err != nil {
		return fmt.Errorf("clear devnet dir: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create devnet dir: %w", err)
	}

	// Consensus peers are baked into genesis as numeric ip:port. Docker mode
	// uses container static IPs (host loopback would resolve to the container
	// itself); local mode uses the host loopback addresses.
	dockerIPs := make([]string, len(vals))
	genesisSockets := make([]string, len(vals))
	for i, v := range vals {
		if docker {
			ip, err := tempoDockerIP(tempoDockerSubnet, i)
			if err != nil {
				return err
			}
			dockerIPs[i] = ip
			genesisSockets[i] = fmt.Sprintf("%s:%d", ip, tempoDockerConsensusPort)
		} else {
			genesisSockets[i] = fmt.Sprintf("%s:%d", v.Host, v.Port)
		}
	}

	// tempo-xtask generate-localnet builds genesis.json plus a per-validator
	// directory (named by its genesis socket) holding signing.key, signing.share,
	// enode.key and enode.identity.
	xtaskBin := spec.TempoXtaskBin
	if xtaskBin == "" {
		xtaskBin = tempoDefaultXtaskBin
	}
	xtaskArgs := append([]string{"generate-localnet", "--output", dataDir, "--force"},
		tempoXtaskGenesisArgs(spec, strings.Join(genesisSockets, ","))...)
	if _, err := chainCmd(ctx, xtaskBin, nil, xtaskArgs...); err != nil {
		return fmt.Errorf("tempo-xtask generate-localnet: %w", err)
	}

	// Rename each validator's socket-named dir to its moniker.
	for i, v := range vals {
		src := filepath.Join(dataDir, genesisSockets[i])
		dst := filepath.Join(dataDir, v.Moniker)
		if src == dst {
			continue
		}
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("clear node dir %s: %w", v.Moniker, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("rename %s -> %s: %w", genesisSockets[i], v.Moniker, err)
		}
	}

	genesisPath := filepath.Join(dataDir, "genesis.json")
	if len(spec.GenesisPatch) > 0 {
		if err := patchTempoGenesis(genesisPath, spec.GenesisPatch); err != nil {
			return err
		}
	}
	// Give each node its own copy of the (patched) genesis.
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return fmt.Errorf("read genesis: %w", err)
	}

	// Trusted peers are read from the enode.identity files xtask wrote. Local
	// mode advertises host:exec-port for every validator (including self);
	// docker mode advertises moniker:exec-port for the others only.
	identities := make([]string, len(vals))
	for i, v := range vals {
		id, err := os.ReadFile(filepath.Join(dataDir, v.Moniker, "enode.identity"))
		if err != nil {
			return fmt.Errorf("read enode identity for %s: %w", v.Moniker, err)
		}
		identities[i] = strings.TrimSpace(string(id))
	}

	tempoBin := spec.TempoBin
	if tempoBin == "" {
		tempoBin = tempoDefaultNodeBin
	}

	for i, v := range vals {
		nodeDir := filepath.Join(dataDir, v.Moniker)
		if err := os.WriteFile(filepath.Join(nodeDir, "genesis.json"), genesisBytes, 0o644); err != nil {
			return fmt.Errorf("copy genesis to %s: %w", v.Moniker, err)
		}
		if err := os.WriteFile(filepath.Join(nodeDir, ".secret"), []byte(tempoLocalnetSecret), 0o644); err != nil {
			return fmt.Errorf("write secret for %s: %w", v.Moniker, err)
		}
		if len(spec.ConfigPatch) > 0 {
			reth, err := toml.Marshal(spec.ConfigPatch)
			if err != nil {
				return fmt.Errorf("encode reth.toml for %s: %w", v.Moniker, err)
			}
			if err := os.WriteFile(filepath.Join(nodeDir, "reth.toml"), reth, 0o644); err != nil {
				return fmt.Errorf("write reth.toml for %s: %w", v.Moniker, err)
			}
		}

		var (
			nodeArgs []string
			script   string
		)
		if docker {
			nodeArgs = tempoNodeArgs(tempoDockerConsensusPort,
				"0.0.0.0", "0.0.0.0", "0.0.0.0",
				tempoDockerTrustedPeers(vals, identities, i), true)
			script = tempoRunScript(tempoBin, nodeArgs, true)
			if err := writeExecutable(filepath.Join(nodeDir, "docker-run.sh"), script); err != nil {
				return err
			}
		} else {
			nodeArgs = tempoNodeArgs(v.Port,
				v.Host, v.Host, "0.0.0.0",
				tempoLocalTrustedPeers(vals, identities), false)
			script = tempoRunScript(tempoBin, nodeArgs, false)
			if err := writeExecutable(filepath.Join(nodeDir, "run.sh"), script); err != nil {
				return err
			}
		}
	}

	if docker {
		if err := writeTempoCompose(spec, vals, dockerIPs, dataDir); err != nil {
			return err
		}
	}
	return nil
}

// tempoXtaskGenesisArgs builds the `tempo-xtask generate-localnet` genesis args
// for the fields the benchmark sets; the rest keep tempo-xtask's own defaults.
func tempoXtaskGenesisArgs(spec messages.BenchmarkSpec, validatorsArg string) []string {
	epoch := spec.TempoEpochLength
	if epoch <= 0 {
		epoch = tempoDefaultEpochLength
	}
	gasLimit := spec.TempoGasLimit
	if gasLimit <= 0 {
		gasLimit = tempoDefaultGasLimit
	}
	return []string{
		"--chain-id", strconv.FormatInt(spec.EVMChainID, 10),
		"--accounts", strconv.Itoa(tempoFundedAccounts(spec)),
		"--epoch-length", strconv.Itoa(epoch),
		"--gas-limit", strconv.FormatInt(gasLimit, 10),
		"--mnemonic", spec.BaseMnemonic,
		"--validators", validatorsArg,
	}
}

// tempoNodeArgs builds the `tempo node` argument list (everything after the
// binary). All paths are node-dir-relative; the launcher cd's there first.
func tempoNodeArgs(base int, listenAddr, metricsAddr, rpcAddr string, trustedPeers []string, dockerBootnodes bool) []string {
	args := []string{
		"node",
		"--consensus.signing-key", "./signing.key",
		"--consensus.secret", "./.secret",
		"--consensus.signing-share", "./signing.share",
		"--consensus.listen-address", fmt.Sprintf("%s:%d", listenAddr, base),
		"--consensus.metrics-address", fmt.Sprintf("%s:%d", metricsAddr, base+2),
		"--chain", "./genesis.json",
		"--datadir", ".",
		"--port", strconv.Itoa(base + 1),
		"--discovery.port", strconv.Itoa(base + 1),
		"--p2p-secret-key", "./enode.key",
		"--trusted-peers", strings.Join(trustedPeers, ","),
		"--authrpc.port", strconv.Itoa(base + 3),
		"--http",
		"--http.addr", rpcAddr,
		"--http.port", strconv.Itoa(base + 4),
		"--http.api", "all",
		"--ws",
		"--ws.addr", rpcAddr,
		"--ws.port", strconv.Itoa(base + 5),
		"--consensus.use-local-defaults",
		"--consensus.allow-private-ips",
	}
	if dockerBootnodes {
		args = append(args, "--tempo.bootnodes-endpoint", "none")
	}
	return args
}

// tempoLocalTrustedPeers advertises every validator (self included) at its host
// loopback address and execution-p2p port.
func tempoLocalTrustedPeers(vals []tempoDevnetValidator, identities []string) []string {
	peers := make([]string, 0, len(vals))
	for i, v := range vals {
		peers = append(peers, fmt.Sprintf("enode://%s@%s:%d", identities[i], v.Host, v.Port+1))
	}
	return peers
}

// tempoDockerTrustedPeers advertises the OTHER validators by docker service
// name at the fixed execution-p2p port (self is excluded).
func tempoDockerTrustedPeers(vals []tempoDevnetValidator, identities []string, self int) []string {
	peers := make([]string, 0, len(vals)-1)
	for i, v := range vals {
		if i == self {
			continue
		}
		peers = append(peers, fmt.Sprintf("enode://%s@%s:%d", identities[i], v.Moniker, tempoDockerConsensusPort+1))
	}
	return peers
}

// tempoRunScript renders the `run.sh`/`docker-run.sh` launcher: an `exec`'d
// command with the binary and each argument single-quoted on its own continued
// line. binary is the exec target; nodeArgs is the argv after it (starting with
// "node").
func tempoRunScript(binary string, nodeArgs []string, docker bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if docker {
		b.WriteString("# auto-generated by evm-benchmark (docker)\n\n")
		b.WriteString("set -eu\n\n")
	} else {
		b.WriteString("# auto-generated by evm-benchmark\n\n")
	}
	b.WriteString("cd \"$(dirname \"$0\")\"\n\n")
	b.WriteString("exec " + shQuote(binary) + " \\\n")
	for i, a := range nodeArgs {
		b.WriteString("  " + shQuote(a))
		if i < len(nodeArgs)-1 {
			b.WriteString(" \\")
		}
		b.WriteString("\n")
	}
	return b.String()
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

// tempoDockerIP returns the static IP for validator index on the docker bridge
// subnet.
func tempoDockerIP(subnet string, index int) (string, error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", fmt.Errorf("parse docker subnet %q: %w", subnet, err)
	}
	base := ipnet.IP.To4()
	if base == nil {
		return "", fmt.Errorf("docker subnet %q is not IPv4", subnet)
	}
	v := binary.BigEndian.Uint32(base) + uint32(tempoDockerIPHostOctetBase+index)
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], v)
	return net.IP(out[:]).String(), nil
}

// patchTempoGenesis deep-merges patch into the genesis file: objects merge
// recursively, everything else is replaced.
func patchTempoGenesis(genesisPath string, patch map[string]interface{}) error {
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

// --- docker-compose generation (single-network) ---

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks"`
}

type composeService struct {
	Image       string                       `yaml:"image"`
	Entrypoint  []string                     `yaml:"entrypoint"`
	Command     string                       `yaml:"command"`
	Volumes     []string                     `yaml:"volumes"`
	Ports       []string                     `yaml:"ports"`
	Healthcheck composeHealthcheck           `yaml:"healthcheck"`
	Restart     string                       `yaml:"restart"`
	Networks    map[string]composeNetworkRef `yaml:"networks"`
}

type composeHealthcheck struct {
	Test        []string `yaml:"test"`
	Interval    string   `yaml:"interval"`
	Retries     int      `yaml:"retries"`
	StartPeriod string   `yaml:"start_period"`
}

type composeNetworkRef struct {
	IPv4Address string `yaml:"ipv4_address"`
}

type composeNetwork struct {
	Driver string      `yaml:"driver"`
	IPAM   composeIPAM `yaml:"ipam"`
}

type composeIPAM struct {
	Config []map[string]string `yaml:"config"`
}

// writeTempoCompose writes a single-network docker-compose.yaml for the
// generated validators.
func writeTempoCompose(spec messages.BenchmarkSpec, vals []tempoDevnetValidator, dockerIPs []string, dataDir string) error {
	network := tempoDockerNetwork(spec)
	image := spec.TempoDockerImage
	containerHTTP := tempoDockerConsensusPort + 4
	containerWS := tempoDockerConsensusPort + 5

	services := make(map[string]composeService, len(vals))
	for i, v := range vals {
		services[v.Moniker] = composeService{
			Image:      image,
			Entrypoint: []string{"/bin/sh"},
			Command:    tempoContainerDataDir + "/docker-run.sh",
			Volumes:    []string{fmt.Sprintf("%s:%s", filepath.Join(dataDir, v.Moniker), tempoContainerDataDir)},
			Ports: []string{
				fmt.Sprintf("%d:%d", v.Port+4, containerHTTP),
				fmt.Sprintf("%d:%d", v.Port+5, containerWS),
			},
			Healthcheck: composeHealthcheck{
				Test:        []string{"CMD-SHELL", fmt.Sprintf("/bin/bash -c 'exec 3<>/dev/tcp/localhost/%d'", containerHTTP)},
				Interval:    "3s",
				Retries:     20,
				StartPeriod: "5s",
			},
			Restart:  "unless-stopped",
			Networks: map[string]composeNetworkRef{network: {IPv4Address: dockerIPs[i]}},
		}
	}

	compose := composeFile{
		Services: services,
		Networks: map[string]composeNetwork{
			network: {
				Driver: "bridge",
				IPAM:   composeIPAM{Config: []map[string]string{{"subnet": tempoDockerSubnet}}},
			},
		},
	}

	out, err := yaml.Marshal(compose)
	if err != nil {
		return fmt.Errorf("encode docker-compose: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "docker-compose.yaml"), out, 0o644); err != nil {
		return fmt.Errorf("write docker-compose: %w", err)
	}
	return nil
}
