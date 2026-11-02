package config

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-jsonnet"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"go.temporal.io/sdk/client"
	"gopkg.in/yaml.v3"
)

type chainConfigRecord struct {
	Cmd           string                 `json:"cmd"`
	ChainID       string                 `json:"chain_id"`
	AccountPrefix string                 `json:"account-prefix"`
	EVMDenom      string                 `json:"evm_denom"`
	EVMChainID    int64                  `json:"evm_chain_id"`
	Bank          map[string]interface{} `json:"bank"`
	EVM           map[string]interface{} `json:"evm"`
}

type TemporalConfig struct {
	HostPort  string `yaml:"host_port"`
	Namespace string `yaml:"namespace"`
	TaskQueue string `yaml:"task_queue"`
}

// Dial connects to the Temporal server the config names.
func (c TemporalConfig) Dial() (client.Client, error) {
	return client.Dial(client.Options{
		HostPort:  c.HostPort,
		Namespace: c.Namespace,
		ConnectionOptions: client.ConnectionOptions{
			DisableKeepAliveCheck:               true,
			DisableKeepAlivePermitWithoutStream: true,
			GetSystemInfoTimeout:                15 * time.Second,
		},
	})
}

type StartConfig struct {
	WorkflowID string `yaml:"workflow_id"`
}

type AppConfig struct {
	Temporal  TemporalConfig         `yaml:"temporal"`
	Start     StartConfig            `yaml:"start"`
	Benchmark messages.BenchmarkSpec `yaml:"benchmark"`
}

func Load(path string) (AppConfig, error) {
	return load(path, true)
}

func LoadForGenerate(path string) (AppConfig, error) {
	return load(path, false)
}

func load(path string, runtimeValidation bool) (AppConfig, error) {
	content, err := readConfig(path)
	if err != nil {
		return AppConfig{}, err
	}

	var cfg AppConfig
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return AppConfig{}, fmt.Errorf("decode config: %w", err)
	}
	expandSpecPaths(&cfg.Benchmark)

	if err := applyChainConfig(path, &cfg); err != nil {
		return AppConfig{}, err
	}

	applyDefaults(&cfg)
	if err := validate(cfg, runtimeValidation); err != nil {
		return AppConfig{}, err
	}

	return cfg, nil
}

// readConfig returns the config as YAML. A .jsonnet config is evaluated first;
// the JSON it produces is YAML too, so both formats decode through the same
// struct tags.
func readConfig(path string) ([]byte, error) {
	if filepath.Ext(path) != ".jsonnet" {
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		return content, nil
	}
	out, err := jsonnet.MakeVM().EvaluateFile(path)
	if err != nil {
		return nil, fmt.Errorf("evaluate config: %w", err)
	}
	return []byte(out), nil
}

// expandPath resolves a leading ~ and $VAR / ${VAR} references in a config path.
func expandPath(p string) string {
	if p == "" {
		return p
	}
	p = os.ExpandEnv(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// expandSpecPaths expands ~ / $VAR in the spec's filesystem-path fields.
func expandSpecPaths(spec *messages.BenchmarkSpec) {
	spec.DataDir = expandPath(spec.DataDir)
	spec.OutDir = expandPath(spec.OutDir)
	spec.Binary = expandPath(spec.Binary)
	spec.TempoBin = expandPath(spec.TempoBin)
	spec.TempoXtaskBin = expandPath(spec.TempoXtaskBin)
	spec.AllegroBin = expandPath(spec.AllegroBin)
	spec.AllegroXtaskBin = expandPath(spec.AllegroXtaskBin)
	spec.RethBin = expandPath(spec.RethBin)
	spec.ChainsConfigPath = expandPath(spec.ChainsConfigPath)
	spec.PatchImage.SourceDir = expandPath(spec.PatchImage.SourceDir)
	// Not paths, but read from the environment too, so a public-network config
	// can be committed without its mnemonic or a keyed provider URL.
	spec.RemoteRPCURL = os.ExpandEnv(spec.RemoteRPCURL)
	spec.BaseMnemonic = os.ExpandEnv(spec.BaseMnemonic)
}

func applyChainConfig(configPath string, cfg *AppConfig) error {
	spec := &cfg.Benchmark
	// Chain profiles (config/chains.jsonnet) are cosmos-sdk-shaped. Non-cosmos
	// families describe their network in the spec itself, so an inherited
	// CHAIN_CONFIG from the environment must not be applied to them.
	if spec.ChainFamily != "" && spec.ChainFamily != "cosmos" {
		spec.ChainConfig = ""
		return nil
	}
	if envChain := os.Getenv("CHAIN_CONFIG"); envChain != "" {
		spec.ChainConfig = envChain
	}
	if spec.ChainConfig == "" {
		return nil
	}

	chainsPathFromEnv := false
	if envPath := os.Getenv("CHAINS_CONFIG_PATH"); envPath != "" {
		spec.ChainsConfigPath = envPath
		chainsPathFromEnv = true
	}
	if spec.ChainsConfigPath == "" {
		return fmt.Errorf("benchmark.chain_config=%q set but benchmark.chains_config_path (or CHAINS_CONFIG_PATH) is empty", spec.ChainConfig)
	}

	chainsPath := spec.ChainsConfigPath
	if !filepath.IsAbs(chainsPath) {
		if chainsPathFromEnv {
			if wd, err := os.Getwd(); err == nil {
				chainsPath = filepath.Clean(filepath.Join(wd, chainsPath))
			}
		} else {
			baseDir := filepath.Dir(configPath)
			chainsPath = filepath.Clean(filepath.Join(baseDir, chainsPath))
		}
	}

	if _, err := os.Stat(chainsPath); err != nil {
		return fmt.Errorf("chains config file %q: %w", chainsPath, err)
	}
	// Store normalized absolute path so downstream workflow/activity execution
	// does not depend on the worker process current working directory.
	spec.ChainsConfigPath = chainsPath

	out, err := evaluateChainProfile(chainsPath, spec.ChainConfig)
	if err != nil {
		return err
	}

	var record chainConfigRecord
	if err := json.Unmarshal(out, &record); err != nil {
		return fmt.Errorf("decode chain config json: %w", err)
	}

	if record.Cmd != "" {
		spec.Binary = record.Cmd
	}
	if record.ChainID != "" {
		spec.ChainID = record.ChainID
	}
	if record.AccountPrefix != "" {
		spec.AddressPrefix = record.AccountPrefix
	}
	if record.EVMDenom != "" {
		spec.Denom = record.EVMDenom
	}
	if record.EVMChainID != 0 {
		spec.EVMChainID = record.EVMChainID
	}

	appState := map[string]interface{}{}
	if record.Bank != nil {
		appState["bank"] = record.Bank
	}
	if record.EVM != nil {
		appState["evm"] = record.EVM
	}
	if len(appState) > 0 {
		spec.GenesisPatch = DeepMerge(map[string]interface{}{"app_state": appState}, spec.GenesisPatch)
	}

	return nil
}

// evaluateChainProfile returns the profile called name from the chains jsonnet
// file at chainsPath, as JSON. Only that profile is evaluated.
func evaluateChainProfile(chainsPath, name string) ([]byte, error) {
	vm := jsonnet.MakeVM()
	vm.ExtVar("CHAIN_CONFIG", name)
	out, err := vm.EvaluateAnonymousSnippet("chain_config", fmt.Sprintf("(import %q)[std.extVar('CHAIN_CONFIG')]", chainsPath))
	if err != nil {
		return nil, fmt.Errorf("resolve chain config %q: %w", name, err)
	}
	return []byte(out), nil
}

// DeepMerge merges src into dst and returns dst: objects merge recursively, any
// other value replaces dst's. Objects src adds are copied, so one patch can be
// applied to several documents.
func DeepMerge(dst, src map[string]interface{}) map[string]interface{} {
	for k, sv := range src {
		if svMap, ok := sv.(map[string]interface{}); ok {
			dvMap, _ := dst[k].(map[string]interface{})
			if dvMap == nil {
				dvMap = make(map[string]interface{}, len(svMap))
			}
			dst[k] = DeepMerge(dvMap, svMap)
			continue
		}
		dst[k] = sv
	}
	return dst
}

func applyDefaults(cfg *AppConfig) {
	cfg.Temporal.HostPort = cmp.Or(cfg.Temporal.HostPort, "127.0.0.1:7233")
	cfg.Temporal.Namespace = cmp.Or(cfg.Temporal.Namespace, "default")
	cfg.Temporal.TaskQueue = cmp.Or(cfg.Temporal.TaskQueue, messages.DefaultTaskQueue)
	cfg.Start.WorkflowID = cmp.Or(cfg.Start.WorkflowID, "evm-benchmark")

	spec := &cfg.Benchmark
	spec.RunnerType = cmp.Or(spec.RunnerType, "local")
	spec.PatchImage.FromImage = cmp.Or(spec.PatchImage.FromImage, spec.DockerImage)
	spec.PatchImage.Dest = cmp.Or(spec.PatchImage.Dest, "/data")
	spec.DockerNetwork = cmp.Or(spec.DockerNetwork, "evm-benchmark")
	spec.HostnameTemplate = cmp.Or(spec.HostnameTemplate, "testplan-%d")
	spec.ChainID = cmp.Or(spec.ChainID, "evm-benchmark-1")
	spec.AddressPrefix = cmp.Or(spec.AddressPrefix, "mantra")
	spec.Denom = cmp.Or(spec.Denom, "amantra")
	spec.Validators = cmp.Or(spec.Validators, 1)
	spec.Fullnodes = max(spec.Fullnodes, 0)
	spec.NumAccounts = cmp.Or(spec.NumAccounts, 100)
	spec.NumTxs = cmp.Or(spec.NumTxs, 20)
	spec.NumIdle = cmp.Or(spec.NumIdle, 20)
	spec.BroadcastConcurrency = cmp.Or(spec.BroadcastConcurrency, 32)
	spec.BroadcastRequestTimeoutSeconds = cmp.Or(spec.BroadcastRequestTimeoutSeconds, 10)
	spec.IdlePollIntervalSeconds = cmp.Or(spec.IdlePollIntervalSeconds, 5)
	spec.ChainHaltIntervalSeconds = cmp.Or(spec.ChainHaltIntervalSeconds, 120)
	// Default per family: Tempo rejects native value transfers outright, so
	// the cosmos default would be an immediate error there.
	if spec.TxType == "" {
		if spec.ChainFamily == "tempo" {
			spec.TxType = messages.ERC20TransferTx
		} else {
			spec.TxType = messages.SimpleTransferTx
		}
	}
	spec.EVMChainID = cmp.Or(spec.EVMChainID, 7888)
	spec.GasPriceWei = cmp.Or(spec.GasPriceWei, 1_000_000_000)
	spec.SimpleTransferGas = cmp.Or(spec.SimpleTransferGas, 21_000)
	spec.ERC20TransferGas = cmp.Or(spec.ERC20TransferGas, 51_630)
	// This address is the cosmos test token. Other chain families supply their
	// own default from their runtime (Tempo uses its genesis TIP-20 fee token),
	// so leave the field empty for them rather than pointing every transaction
	// at a contract that does not exist on that chain.
	if spec.ERC20ContractAddress == "" && (spec.ChainFamily == "" || spec.ChainFamily == "cosmos") {
		spec.ERC20ContractAddress = "0x1000000000000000000000000000000000000000"
	}
	spec.RPCPort = cmp.Or(spec.RPCPort, 26657)
	spec.EVMRPCPort = cmp.Or(spec.EVMRPCPort, 8545)
	spec.MinReadyHeight = cmp.Or(spec.MinReadyHeight, 3)
}

func validate(cfg AppConfig, runtimeValidation bool) error {
	if cfg.Benchmark.DataDir == "" {
		return fmt.Errorf("benchmark.data_dir is required")
	}
	if cfg.Benchmark.Validators < 1 {
		return fmt.Errorf("benchmark.validators must be >= 1")
	}
	if cfg.Benchmark.TxType != messages.SimpleTransferTx && cfg.Benchmark.TxType != messages.ERC20TransferTx {
		return fmt.Errorf("benchmark.tx_type must be %q or %q", messages.SimpleTransferTx, messages.ERC20TransferTx)
	}
	if cfg.Benchmark.RunnerType != "local" && cfg.Benchmark.RunnerType != "docker" {
		return fmt.Errorf("benchmark.runner_type must be \"local\" or \"docker\"")
	}
	if err := validateHotRecipient(cfg.Benchmark); err != nil {
		return err
	}
	if err := validateRemote(cfg.Benchmark); err != nil {
		return err
	}

	if !runtimeValidation {
		return nil
	}

	if cfg.Benchmark.RunnerType == "docker" && cfg.Benchmark.StartNode && cfg.Benchmark.DockerImage == "" {
		return fmt.Errorf("benchmark.docker_image is required when runner_type=docker and start_node=true")
	}
	if cfg.Benchmark.PatchImage.Enabled {
		if !cfg.Benchmark.StartNode {
			return fmt.Errorf("benchmark.patch_image.enabled requires benchmark.start_node=true")
		}
		if cfg.Benchmark.RunnerType == "docker" && cfg.Benchmark.PatchImage.FromImage == "" && cfg.Benchmark.DockerImage == "" {
			return fmt.Errorf("benchmark.patch_image.enabled requires benchmark.patch_image.from_image or benchmark.docker_image")
		}
	}
	return nil
}

// validateHotRecipient rejects a hot_recipient_percent the shared signer cannot
// apply. Tempo's native generator signs its own shapes and would ignore it, so
// a contended run there is tempo_tx_shape: hot instead.
func validateHotRecipient(spec messages.BenchmarkSpec) error {
	if spec.HotRecipientPercent < 0 || spec.HotRecipientPercent > 100 {
		return fmt.Errorf("benchmark.hot_recipient_percent must be between 0 and 100 (got %d)", spec.HotRecipientPercent)
	}
	if spec.HotRecipientPercent > 0 && spec.ChainFamily == "tempo" && !spec.TempoLegacyTxs {
		return fmt.Errorf("benchmark.hot_recipient_percent applies to the shared legacy signer; " +
			"tempo's native transactions use tempo_tx_shape: hot")
	}
	return nil
}

// validateRemote rejects the settings a remote run cannot honour. Checked on
// the generate path too, since that is where a remote run funds its accounts.
func validateRemote(spec messages.BenchmarkSpec) error {
	if spec.RemoteRPCURL == "" {
		return nil
	}
	if u, err := url.Parse(spec.RemoteRPCURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("benchmark.remote_rpc_url %q is not an http(s) URL", spec.RemoteRPCURL)
	}
	if spec.ChainFamily != "tempo" {
		// The shared legacy signer starts every account at nonce 0, which only
		// holds on a chain the benchmark just created.
		return fmt.Errorf("benchmark.remote_rpc_url is supported for chain_family: tempo only")
	}
	if spec.StartNode || spec.RunnerType != "local" {
		return fmt.Errorf("benchmark.remote_rpc_url benchmarks a running network; set start_node: false and runner_type: local")
	}
	if spec.Validators != 1 || spec.Fullnodes != 0 {
		return fmt.Errorf("benchmark.remote_rpc_url drives one endpoint from one sender; set validators: 1 and fullnodes: 0 (got %d/%d)",
			spec.Validators, spec.Fullnodes)
	}
	return nil
}
