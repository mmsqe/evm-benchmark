package config

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/mmsqe/evm-benchmark/internal/messages"
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
	content, err := os.ReadFile(path)
	if err != nil {
		return AppConfig{}, fmt.Errorf("read config: %w", err)
	}

	var cfg AppConfig
	if err := yaml.Unmarshal(content, &cfg); err != nil {
		return AppConfig{}, fmt.Errorf("decode config: %w", err)
	}

	if err := applyChainConfig(path, &cfg); err != nil {
		return AppConfig{}, err
	}

	applyDefaults(&cfg)
	if err := validate(cfg, runtimeValidation); err != nil {
		return AppConfig{}, err
	}

	return cfg, nil
}

func applyChainConfig(configPath string, cfg *AppConfig) error {
	spec := &cfg.Benchmark
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

	if _, err := exec.LookPath("jsonnet"); err != nil {
		return fmt.Errorf("jsonnet binary not found in PATH: %w", err)
	}

	query := fmt.Sprintf("local chains = import %q; chains[std.extVar(\"CHAIN_CONFIG\") ]", chainsPath)
	cmd := exec.Command("jsonnet", "--ext-str", "CHAIN_CONFIG="+spec.ChainConfig, "-e", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolve chain config via jsonnet: %w\n%s", err, string(out))
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

	chainGenesisPatch := map[string]interface{}{}
	chainAppState := map[string]interface{}{}
	if record.Bank != nil {
		chainAppState["bank"] = record.Bank
	}
	if record.EVM != nil {
		chainAppState["evm"] = record.EVM
	}
	if len(chainAppState) > 0 {
		chainGenesisPatch["app_state"] = chainAppState
	}
	if len(chainGenesisPatch) > 0 {
		mergedGenesisPatch := map[string]interface{}{}
		mergeMaps(mergedGenesisPatch, chainGenesisPatch)
		mergeMaps(mergedGenesisPatch, spec.GenesisPatch)
		spec.GenesisPatch = mergedGenesisPatch
	}

	return nil
}

func mergeMaps(dst, src map[string]interface{}) {
	if src == nil {
		return
	}
	for k, v := range src {
		if srcMap, ok := v.(map[string]interface{}); ok {
			if dstMap, ok := dst[k].(map[string]interface{}); ok {
				mergeMaps(dstMap, srcMap)
				dst[k] = dstMap
			} else {
				next := map[string]interface{}{}
				mergeMaps(next, srcMap)
				dst[k] = next
			}
			continue
		}
		dst[k] = v
	}
}

func applyDefaults(cfg *AppConfig) {
	if cfg.Temporal.HostPort == "" {
		cfg.Temporal.HostPort = "127.0.0.1:7233"
	}
	if cfg.Temporal.Namespace == "" {
		cfg.Temporal.Namespace = "default"
	}
	if cfg.Temporal.TaskQueue == "" {
		cfg.Temporal.TaskQueue = messages.DefaultTaskQueue
	}
	if cfg.Start.WorkflowID == "" {
		cfg.Start.WorkflowID = "evm-benchmark"
	}

	spec := &cfg.Benchmark
	if spec.RunnerType == "" {
		spec.RunnerType = "local"
	}
	if spec.PatchImageFromImage == "" {
		spec.PatchImageFromImage = spec.DockerImage
	}
	if spec.PatchImageDest == "" {
		spec.PatchImageDest = "/data"
	}
	if spec.DockerNetwork == "" {
		spec.DockerNetwork = "evm-benchmark"
	}
	if !spec.DockerCreateNetwork {
		spec.DockerCreateNetwork = true
	}
	if spec.HostnameTemplate == "" {
		spec.HostnameTemplate = "testplan-%d"
	}
	if spec.ChainID == "" {
		spec.ChainID = "evm-benchmark-1"
	}
	if spec.AddressPrefix == "" {
		spec.AddressPrefix = "mantra"
	}
	if spec.Denom == "" {
		spec.Denom = "amantra"
	}
	if spec.RPCURLTemplate == "" {
		spec.RPCURLTemplate = "http://%s:8545"
	}
	if spec.TendermintURLTemplate == "" {
		spec.TendermintURLTemplate = "http://%s:26657"
	}
	if spec.Validators == 0 {
		spec.Validators = 1
	}
	if spec.Fullnodes < 0 {
		spec.Fullnodes = 0
	}
	if spec.NumAccounts == 0 {
		spec.NumAccounts = 100
	}
	if spec.NumTxs == 0 {
		spec.NumTxs = 20
	}
	if spec.NumIdle == 0 {
		spec.NumIdle = 20
	}
	if spec.IdlePollIntervalSeconds == 0 {
		spec.IdlePollIntervalSeconds = 5
	}
	if spec.ChainHaltIntervalSeconds == 0 {
		spec.ChainHaltIntervalSeconds = 120
	}
	if spec.TxType == "" {
		spec.TxType = messages.SimpleTransferTx
	}
	if spec.BatchSize == 0 {
		spec.BatchSize = 1
	}
	if spec.EVMChainID == 0 {
		spec.EVMChainID = 7888
	}
	if spec.GasPriceWei == 0 {
		spec.GasPriceWei = 1_000_000_000
	}
	if spec.SimpleTransferGas == 0 {
		spec.SimpleTransferGas = 21_000
	}
	if spec.ERC20TransferGas == 0 {
		spec.ERC20TransferGas = 51_630
	}
	if spec.ERC20ContractAddress == "" {
		spec.ERC20ContractAddress = "0x1000000000000000000000000000000000000000"
	}
	if spec.RPCPort == 0 {
		spec.RPCPort = 26657
	}
	if spec.EVMRPCPort == 0 {
		spec.EVMRPCPort = 8545
	}
	if spec.MinReadyHeight == 0 {
		spec.MinReadyHeight = 3
	}
	if spec.PeerReadyTimeoutSeconds == 0 {
		spec.PeerReadyTimeoutSeconds = 2400
	}
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

	if !runtimeValidation {
		return nil
	}

	if cfg.Benchmark.RunnerType == "docker" && cfg.Benchmark.StartNode && cfg.Benchmark.DockerImage == "" {
		return fmt.Errorf("benchmark.docker_image is required when runner_type=docker and start_node=true")
	}
	if cfg.Benchmark.PatchImageEnabled {
		if !cfg.Benchmark.StartNode {
			return fmt.Errorf("benchmark.patch_image_enabled requires benchmark.start_node=true")
		}
		if cfg.Benchmark.RunnerType == "docker" && cfg.Benchmark.PatchImageFromImage == "" && cfg.Benchmark.DockerImage == "" {
			return fmt.Errorf("benchmark.patch_image_enabled requires benchmark.patch_image_from_image or benchmark.docker_image")
		}
	}
	return nil
}
