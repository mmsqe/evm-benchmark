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
	Validators               int                    `yaml:"validators"`
	Fullnodes                int                    `yaml:"fullnodes"`
	NumAccounts              int                    `yaml:"num_accounts"`
	NumTxs                   int                    `yaml:"num_txs"`
	NumIdle                  int                    `yaml:"num_idle"`
	IdlePollIntervalSeconds  int                    `yaml:"idle_poll_interval_seconds"`
	ChainHaltIntervalSeconds int                    `yaml:"chain_halt_interval_seconds"`
	TxType                   TxType                 `yaml:"tx_type"`
	EVMChainID               int64                  `yaml:"evm_chain_id"`
	GasPriceWei              int64                  `yaml:"gas_price_wei"`
	SimpleTransferGas        uint64                 `yaml:"simple_transfer_gas"`
	ERC20TransferGas         uint64                 `yaml:"erc20_transfer_gas"`
	ERC20ContractAddress     string                 `yaml:"erc20_contract_address"`
	RPCPort                  int                    `yaml:"rpc_port"`
	EVMRPCPort               int                    `yaml:"evm_rpc_port"`

	// RemoteRPCURL sends to an already-running network, such as a public
	// testnet, instead of generating one. Tempo only.
	RemoteRPCURL string `yaml:"remote_rpc_url"`

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
	// With remote_rpc_url: fund senders short of the run's fees through the
	// endpoint's tempo_fundAddress faucet (testnets only).
	TempoFaucet bool `yaml:"tempo_faucet"`
	// With remote_rpc_url: fund short senders from account 0 of base_mnemonic.
	TempoFundFromIndex0 bool `yaml:"tempo_fund_from_index0"`
	// Docker mode: the devnet is generated with a compose file and the whole
	// cluster is started by `docker compose up`, rather than one container per
	// node as the cosmos runner does.
	TempoDockerImage    string `yaml:"tempo_docker_image"`
	TempoDockerNetwork  string `yaml:"tempo_docker_network"`
	TempoComposeProject string `yaml:"tempo_compose_project"`

	// Allegro-family settings (chain_family: allegro). Allegro embeds a reth
	// execution node inside a commonware simplex consensus engine, so it speaks
	// plain EVM JSON-RPC and the shared signer/stats path applies unchanged; the
	// network is generated with `allegro-xtask genesis`.
	AllegroBin      string `yaml:"allegro_bin"`
	AllegroXtaskBin string `yaml:"allegro_xtask_bin"`
	// Base of each node's 4-port block (consensus p2p, execution p2p, authrpc,
	// http). Node N's JSON-RPC is at base + N*4 + 3.
	AllegroBasePort int `yaml:"allegro_base_port"`
	// Per-block gas budget, written to genesis and pinned as the builder target.
	// It caps a block at gas_limit/21000 transfers, so it bounds TPS directly —
	// set it deliberately and report it alongside any result. 0 keeps
	// allegro-xtask's own default (30M).
	AllegroGasLimit        int64 `yaml:"allegro_gas_limit"`
	AllegroLeaderTimeoutMS int   `yaml:"allegro_leader_timeout_ms"`
	AllegroCertTimeoutMS   int   `yaml:"allegro_cert_timeout_ms"`
	// Extra flags appended verbatim to every generated `allegro node` launcher,
	// e.g. ["--builder.interval", "200ms"].
	AllegroNodeArgs []string `yaml:"allegro_node_args"`

	// Reth-family settings (chain_family: reth); see "Reth Mode" in the README.
	RethBin string `yaml:"reth_bin"`
	// Base of each node's 5-port block (p2p, authrpc, http, ws, metrics).
	RethBasePort int `yaml:"reth_base_port"`
	// Per-block gas budget for genesis and the builder; 0 keeps a 30M genesis
	// limit and reth's own builder target.
	RethGasLimit int64 `yaml:"reth_gas_limit"`
	// Dev block interval in Go duration syntax, at least 1s; default 1s.
	RethBlockTime string `yaml:"reth_block_time"`
	// Extra flags for every `reth node` launcher.
	RethNodeArgs []string `yaml:"reth_node_args"`
	// Extra flags for fullnode launchers only, after reth_node_args.
	RethFullnodeArgs []string `yaml:"reth_fullnode_args"`

	// Percentage (0-100) of each account's transactions the shared signer sends
	// to bench.HotRecipient instead of back to the sender.
	HotRecipientPercent int `yaml:"hot_recipient_percent"`

	MinReadyHeight        int64 `yaml:"min_ready_height"`
	PreGenerateTxs        bool  `yaml:"pre_generate_txs"`
	RunNodes              bool  `yaml:"run_nodes"`
	ValidatorGenerateLoad bool  `yaml:"validator_generate_load"`
	BroadcastConcurrency  int   `yaml:"broadcast_concurrency"`
	// Pause sending while more than this many transactions are pending. Negative
	// never pauses; 0 uses bench.DefaultPendingWatermark.
	BroadcastPendingWatermark int64 `yaml:"broadcast_pending_watermark"`
	// Transactions per JSON-RPC request, 1 by default. Above 1 the node must
	// support JSON-RPC batch requests.
	BroadcastBatchSize int `yaml:"broadcast_batch_size"`
	// How long one request may take. 10 (the default) suits a local node; a
	// node that answers requests one at a time makes each wait for those ahead
	// of it, so a remote run with many workers needs more.
	BroadcastRequestTimeoutSeconds int      `yaml:"broadcast_request_timeout_seconds"`
	StartNode                      bool     `yaml:"start_node"`
	StartArgs                      []string `yaml:"start_args"`
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
}

// LayoutRequest is the input of GenerateLayout and LoadLayout.
type LayoutRequest struct {
	Spec BenchmarkSpec
}

type LayoutResponse struct {
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

// NodeRunResult reports one node's run; its rates are bench.RunStats's.
type NodeRunResult struct {
	GlobalSeq     int
	TxsSent       int
	IncludedTxs   int
	FullSecondTPS float64
	FullSeconds   int
	SustainedTPS  float64
	ActiveSeconds int
	DrainTPS      float64
	DrainSeconds  int
	PendingTxpool int64
	TopTPS        []float64
	StatsFile     string
}
