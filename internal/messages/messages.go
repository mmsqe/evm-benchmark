package messages

const DefaultTaskQueue = "EVM_BENCHMARK_TASK_QUEUE"

type TxType string

const (
	SimpleTransferTx TxType = "simple-transfer"
	ERC20TransferTx  TxType = "erc20-transfer"
)

type BenchmarkSpec struct {
	DataDir                  string                 `yaml:"data_dir"`
	OutDir                   string                 `yaml:"out_dir"`
	SkipGenerateLayout       bool                   `yaml:"skip_generate_layout"`
	RunnerType               string                 `yaml:"runner_type"`
	DockerImage              string                 `yaml:"docker_image"`
	DockerVolumes            []string               `yaml:"docker_volumes"`
	DockerKeepContainers     bool                   `yaml:"docker_keep_containers"`
	DockerEnv                map[string]string      `yaml:"docker_env"`
	PatchImage               PatchImageConfig       `yaml:"patch_image"`
	DockerNetwork            string                 `yaml:"docker_network"`
	DockerCreateNetwork      bool                   `yaml:"docker_create_network"`
	ChainFamily              string                 `yaml:"chain_family"`
	ChainConfig              string                 `yaml:"chain_config"`
	ChainsConfigPath         string                 `yaml:"chains_config_path"`
	ConfigPatch              map[string]interface{} `yaml:"config_patch"`
	AppPatch                 map[string]interface{} `yaml:"app_patch"`
	GenesisPatch             map[string]interface{} `yaml:"genesis_patch"`
	Binary                   string                 `yaml:"binary"`
	ChainID                  string                 `yaml:"chain_id"`
	AddressPrefix            string                 `yaml:"address_prefix"`
	Denom                    string                 `yaml:"denom"`
	BaseMnemonic             string                 `yaml:"base_mnemonic"`
	HostnameTemplate         string                 `yaml:"hostname_template"`
	RPCURLTemplate           string                 `yaml:"rpc_url_template"`
	TendermintURLTemplate    string                 `yaml:"tendermint_url_template"`
	Validators               int                    `yaml:"validators"`
	Fullnodes                int                    `yaml:"fullnodes"`
	NumAccounts              int                    `yaml:"num_accounts"`
	NumTxs                   int                    `yaml:"num_txs"`
	NumIdle                  int                    `yaml:"num_idle"`
	IdlePollIntervalSeconds  int                    `yaml:"idle_poll_interval_seconds"`
	ChainHaltIntervalSeconds int                    `yaml:"chain_halt_interval_seconds"`
	TxType                   TxType                 `yaml:"tx_type"`
	BatchSize                int                    `yaml:"batch_size"`
	EVMChainID               int64                  `yaml:"evm_chain_id"`
	GasPriceWei              int64                  `yaml:"gas_price_wei"`
	SimpleTransferGas        uint64                 `yaml:"simple_transfer_gas"`
	ERC20TransferGas         uint64                 `yaml:"erc20_transfer_gas"`
	ERC20ContractAddress     string                 `yaml:"erc20_contract_address"`
	RPCPort                  int                    `yaml:"rpc_port"`
	EVMRPCPort               int                    `yaml:"evm_rpc_port"`

	// Tempo-family settings (chain_family: tempo). Tempo networks are generated
	// in-process via `tempo-xtask generate-localnet` rather than a chain binary.
	TempoBin         string `yaml:"tempo_bin"`
	TempoXtaskBin    string `yaml:"tempo_xtask_bin"`
	TempoBasePort    int    `yaml:"tempo_base_port"`
	TempoEpochLength int    `yaml:"tempo_epoch_length"`
	TempoGasLimit    int64  `yaml:"tempo_gas_limit"`
	// Extra flags appended verbatim to every generated `tempo node` launcher.
	// Use to tune consensus knobs the benchmark does not model, e.g. raising the
	// per-block build budget with ["--consensus.target-block-time", "2s"].
	TempoNodeArgs []string `yaml:"tempo_node_args"`

	// Native (0x76) transaction generation. Tempo's native envelope is signed
	// in-process (internal/tempotx) and is the default. Set TempoLegacyTxs to
	// fall back to the shared legacy/London signer (single-validator only).
	TempoLegacyTxs            bool  `yaml:"tempo_legacy_txs"`
	TempoMaxPriorityFeePerGas int64 `yaml:"tempo_max_priority_fee_per_gas"`
	TempoNonceKey             int   `yaml:"tempo_nonce_key"`
	// Spread each account's txs round-robin across this many 2D-nonce lanes
	// (nonce_key = tempo_nonce_key + lane), each lane sequential. 1 (default) is
	// plain sequential nonces; >1 lets one owner issue parallel-eligible txs —
	// with tx_shape=approve, the conflict workload on the allowance slot.
	TempoNonceLanes int `yaml:"tempo_nonce_lanes"`
	// Workload shape for the native generator: self (uncontended), hot (every
	// sender writes one recipient's balance), noop (execution floor) or batch
	// (several calls per transaction). Empty means the generator's default.
	TempoTxShape    string `yaml:"tempo_tx_shape"`
	TempoBatchCalls int    `yaml:"tempo_batch_calls"`
	// Docker mode: the devnet is generated with a compose file and the whole
	// cluster is started by `docker compose up`, rather than one container per
	// node as the cosmos runner does.
	TempoDockerImage        string   `yaml:"tempo_docker_image"`
	TempoDockerNetwork      string   `yaml:"tempo_docker_network"`
	TempoComposeProject     string   `yaml:"tempo_compose_project"`
	MinReadyHeight          int64    `yaml:"min_ready_height"`
	PeerReadyTimeoutSeconds int      `yaml:"peer_ready_timeout_seconds"`
	PreGenerateTxs          bool     `yaml:"pre_generate_txs"`
	RunNodes                bool     `yaml:"run_nodes"`
	ValidatorGenerateLoad   bool     `yaml:"validator_generate_load"`
	BroadcastConcurrency    int      `yaml:"broadcast_concurrency"`
	StartNode               bool     `yaml:"start_node"`
	StartArgs               []string `yaml:"start_args"`
}

type PatchImageConfig struct {
	Enabled   bool   `yaml:"enabled"`
	FromImage string `yaml:"from_image"`
	ToImage   string `yaml:"to_image"`
	SourceDir string `yaml:"source_dir"`
	Dest      string `yaml:"dest"`
}

type WorkflowRequest struct {
	Spec BenchmarkSpec
}

type WorkflowResponse struct {
	NodeResults []NodeRunResult
}

type NodeTarget struct {
	GlobalSeq      int
	Group          string
	GroupSeq       int
	Hostname       string
	Home           string
	HostRPCPort    int
	HostEVMRPCPort int
	RPCURL         string
	TMRPCURL       string
}

type GenerateLayoutRequest struct {
	Spec BenchmarkSpec
}

type GenerateLayoutResponse struct {
	Nodes []NodeTarget
}

type LoadLayoutRequest struct {
	Spec BenchmarkSpec
}

type LoadLayoutResponse struct {
	Nodes []NodeTarget
}

type GenerateTxsRequest struct {
	Spec   BenchmarkSpec
	Target NodeTarget
}

type RunNodeRequest struct {
	Spec                BenchmarkSpec
	Target              NodeTarget
	DockerImageOverride string
}

type PatchImageRequest struct {
	Spec BenchmarkSpec
}

type PatchImageResponse struct {
	ImageTag   string
	FromImage  string
	SourceDir  string
	TargetDest string
}

type TPSDetail struct {
	Height int64
	Time   string
	Txs    int
	TPS    float64
}

type NodeRunResult struct {
	GlobalSeq     int
	TxsSent       int
	IncludedTxs   int
	PendingTxpool int64
	TopTPS        []float64
	TopTPSDetails []TPSDetail
	StatsFile     string
}
