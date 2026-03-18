package activities

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	tomlv2 "github.com/pelletier/go-toml/v2"
)

type Activity struct{}

type chainConfigRecord struct {
	Bank map[string]interface{} `json:"bank"`
	EVM  map[string]interface{} `json:"evm"`
}

const (
	validatorKeyName      = "validator"
	validatorInitialCoins = "100000000000000000000"
	validatorStakedCoins  = "10000000000000000000"
	accountInitialCoins   = "10000000000000000000000000"
	erc20ContractAddress  = "0x1000000000000000000000000000000000000000"
)

func (a *Activity) GenerateLayout(ctx context.Context, req messages.GenerateLayoutRequest) (messages.GenerateLayoutResponse, error) {
	spec := req.Spec
	if err := enrichGenesisPatchFromChainConfig(&spec); err != nil {
		return messages.GenerateLayoutResponse{}, err
	}
	nodes := make([]messages.NodeTarget, 0, spec.Validators+spec.Fullnodes)

	if err := os.MkdirAll(spec.DataDir, 0o755); err != nil {
		return messages.GenerateLayoutResponse{}, fmt.Errorf("create data dir: %w", err)
	}
	if spec.OutDir != "" {
		if err := os.MkdirAll(spec.OutDir, 0o755); err != nil {
			return messages.GenerateLayoutResponse{}, fmt.Errorf("create output dir: %w", err)
		}
	}

	mkNode := func(group string, groupSeq, globalSeq int) messages.NodeTarget {
		hostname := fmt.Sprintf(spec.HostnameTemplate, globalSeq)
		home := filepath.Join(spec.DataDir, group, fmt.Sprintf("%d", groupSeq))
		hostRPCPort := spec.RPCPort + globalSeq
		hostEVMRPCPort := spec.EVMRPCPort + globalSeq

		rpcURL := fmt.Sprintf("http://127.0.0.1:%d", spec.EVMRPCPort)
		tmURL := fmt.Sprintf("http://127.0.0.1:%d", spec.RPCPort)
		if spec.RunnerType == "docker" {
			rpcURL = fmt.Sprintf("http://127.0.0.1:%d", hostEVMRPCPort)
			tmURL = fmt.Sprintf("http://127.0.0.1:%d", hostRPCPort)
		}
		return messages.NodeTarget{
			GlobalSeq:      globalSeq,
			Group:          group,
			GroupSeq:       groupSeq,
			Hostname:       hostname,
			Home:           home,
			HostRPCPort:    hostRPCPort,
			HostEVMRPCPort: hostEVMRPCPort,
			RPCURL:         rpcURL,
			TMRPCURL:       tmURL,
		}
	}

	for i := 0; i < spec.Validators; i++ {
		n := mkNode("validators", i, i)
		if err := os.MkdirAll(n.Home, 0o755); err != nil {
			return messages.GenerateLayoutResponse{}, fmt.Errorf("create validator home: %w", err)
		}
		nodes = append(nodes, n)
	}
	for i := 0; i < spec.Fullnodes; i++ {
		globalSeq := i + spec.Validators
		n := mkNode("fullnodes", i, globalSeq)
		if err := os.MkdirAll(n.Home, 0o755); err != nil {
			return messages.GenerateLayoutResponse{}, fmt.Errorf("create fullnode home: %w", err)
		}
		nodes = append(nodes, n)
	}

	if err := bootstrapNodesAndGenesis(ctx, spec, nodes); err != nil {
		return messages.GenerateLayoutResponse{}, err
	}

	if err := writeJSON(filepath.Join(spec.DataDir, "config.json"), spec); err != nil {
		return messages.GenerateLayoutResponse{}, err
	}
	if err := writeJSON(filepath.Join(spec.DataDir, "nodes.json"), nodes); err != nil {
		return messages.GenerateLayoutResponse{}, err
	}

	_ = ctx
	return messages.GenerateLayoutResponse{Nodes: nodes}, nil
}

func enrichGenesisPatchFromChainConfig(spec *messages.BenchmarkSpec) error {
	if spec == nil || strings.TrimSpace(spec.ChainConfig) == "" {
		return nil
	}

	chainsPath := strings.TrimSpace(spec.ChainsConfigPath)
	if chainsPath == "" {
		return nil
	}
	if !filepath.IsAbs(chainsPath) {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve chains config path: %w", err)
		}
		chainsPath = filepath.Clean(filepath.Join(wd, chainsPath))
	}
	if _, err := os.Stat(chainsPath); err != nil {
		return fmt.Errorf("chains config file %q: %w", chainsPath, err)
	}

	if _, err := exec.LookPath("jsonnet"); err != nil {
		return fmt.Errorf("jsonnet binary not found in PATH: %w", err)
	}

	query := fmt.Sprintf("local chains = import %q; chains[std.extVar(\"CHAIN_CONFIG\") ]", chainsPath)
	out, err := chainCmd(context.Background(), "jsonnet", nil, "--ext-str", "CHAIN_CONFIG="+spec.ChainConfig, "-e", query)
	if err != nil {
		return fmt.Errorf("resolve chain config via jsonnet: %w", err)
	}

	var record chainConfigRecord
	if err := json.Unmarshal(out, &record); err != nil {
		return fmt.Errorf("decode chain config json: %w", err)
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
	if len(chainGenesisPatch) == 0 {
		return nil
	}

	mergedGenesisPatch := map[string]interface{}{}
	mergeMaps(mergedGenesisPatch, chainGenesisPatch)
	mergeMaps(mergedGenesisPatch, spec.GenesisPatch)
	spec.GenesisPatch = mergedGenesisPatch
	return nil
}

func (a *Activity) GenerateTxs(ctx context.Context, req messages.GenerateTxsRequest) (int, error) {
	txs, err := bench.GenerateSignedTxs(req.Spec, req.Target.GlobalSeq)
	if err != nil {
		return 0, err
	}

	txDir := filepath.Join(req.Spec.DataDir, "txs")
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		return 0, fmt.Errorf("create tx dir: %w", err)
	}
	if err := writeJSON(filepath.Join(txDir, fmt.Sprintf("%d.json", req.Target.GlobalSeq)), txs); err != nil {
		return 0, err
	}
	_ = ctx
	return len(txs), nil
}

func (a *Activity) PatchImage(ctx context.Context, req messages.PatchImageRequest) (messages.PatchImageResponse, error) {
	spec := req.Spec

	fromImage := strings.TrimSpace(spec.PatchImageFromImage)
	if fromImage == "" {
		fromImage = strings.TrimSpace(spec.DockerImage)
	}
	if fromImage == "" {
		return messages.PatchImageResponse{}, fmt.Errorf("patchimage requires patch_image_from_image or docker_image")
	}

	toImage := strings.TrimSpace(spec.PatchImageToImage)
	if toImage == "" {
		toImage = fromImage + "-patched"
	}

	sourceDir := strings.TrimSpace(spec.PatchImageSourceDir)
	if sourceDir == "" {
		outCandidate := filepath.Join(spec.DataDir, "out")
		if st, err := os.Stat(outCandidate); err == nil && st.IsDir() {
			sourceDir = outCandidate
		} else {
			sourceDir = spec.DataDir
		}
	}

	st, err := os.Stat(sourceDir)
	if err != nil {
		return messages.PatchImageResponse{}, fmt.Errorf("patchimage source dir %q: %w", sourceDir, err)
	}
	if !st.IsDir() {
		return messages.PatchImageResponse{}, fmt.Errorf("patchimage source path %q is not a directory", sourceDir)
	}

	dst := strings.TrimSpace(spec.PatchImageDest)
	if dst == "" {
		if spec.RunnerType == "docker" {
			dst = "/data"
		} else {
			dst = spec.DataDir
		}
	}

	if spec.RunnerType != "docker" {
		// For local runs, patchimage semantics mean replacing the runtime data directory
		// with a prepared source layout, analogous to adding ./out into /data in Docker mode.
		if dst == "/data" {
			dst = spec.DataDir
		}

		srcAbs, err := filepath.Abs(sourceDir)
		if err != nil {
			return messages.PatchImageResponse{}, fmt.Errorf("resolve local patch source path: %w", err)
		}
		dstAbs, err := filepath.Abs(dst)
		if err != nil {
			return messages.PatchImageResponse{}, fmt.Errorf("resolve local patch destination path: %w", err)
		}

		if srcAbs != dstAbs {
			if err := os.RemoveAll(dstAbs); err != nil {
				return messages.PatchImageResponse{}, fmt.Errorf("reset local patch destination %q: %w", dstAbs, err)
			}
		}
		if err := copyDir(sourceDir, dstAbs); err != nil {
			return messages.PatchImageResponse{}, fmt.Errorf("copy local patch source dir: %w", err)
		}

		return messages.PatchImageResponse{
			ImageTag:   "",
			FromImage:  fromImage,
			SourceDir:  sourceDir,
			TargetDest: dstAbs,
		}, nil
	}

	tmpDir, err := os.MkdirTemp("", "evm-benchmark-patchimage-")
	if err != nil {
		return messages.PatchImageResponse{}, fmt.Errorf("create patchimage temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	buildOut := filepath.Join(tmpDir, "out")
	if err := copyDir(sourceDir, buildOut); err != nil {
		return messages.PatchImageResponse{}, fmt.Errorf("copy patchimage source dir: %w", err)
	}

	dockerfile := fmt.Sprintf("FROM %s\nADD ./out %s\n", fromImage, dst)
	if err := os.WriteFile(filepath.Join(tmpDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return messages.PatchImageResponse{}, fmt.Errorf("write patchimage Dockerfile: %w", err)
	}

	if _, err := chainCmd(ctx, "docker", nil, "build", "-t", toImage, tmpDir); err != nil {
		return messages.PatchImageResponse{}, fmt.Errorf("build patch image: %w", err)
	}

	return messages.PatchImageResponse{
		ImageTag:   toImage,
		FromImage:  fromImage,
		SourceDir:  sourceDir,
		TargetDest: dst,
	}, nil
}

func (a *Activity) RunNode(ctx context.Context, req messages.RunNodeRequest) (messages.NodeRunResult, error) {
	spec := req.Spec
	target := req.Target
	if target.Group == "validators" && !spec.ValidatorGenerateLoad {
		return messages.NodeRunResult{GlobalSeq: target.GlobalSeq}, nil
	}

	txPath := filepath.Join(spec.DataDir, "txs", fmt.Sprintf("%d.json", target.GlobalSeq))
	var txs []string
	if err := readJSON(txPath, &txs); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("load node txs: %w", err)
	}

	var proc *exec.Cmd
	containerName := ""
	dockerImage := strings.TrimSpace(spec.DockerImage)
	if strings.TrimSpace(req.DockerImageOverride) != "" {
		dockerImage = strings.TrimSpace(req.DockerImageOverride)
	}
	containerHome := "/data"
	usePatchedImageLayout := false
	if strings.TrimSpace(req.DockerImageOverride) != "" {
		containerHome = path.Join("/data", target.Group, fmt.Sprintf("%d", target.GroupSeq))
		usePatchedImageLayout = true
	}
	if spec.StartNode {
		if spec.Binary == "" {
			return messages.NodeRunResult{}, fmt.Errorf("benchmark.binary is required when start_node=true")
		}

		if spec.RunnerType == "docker" {
			if spec.DockerCreateNetwork {
				_, _ = chainCmd(ctx, "docker", nil, "network", "create", spec.DockerNetwork)
			}

			containerName = fmt.Sprintf("evm-benchmark-%d", target.GlobalSeq)
			binaryInImage := spec.Binary
			if strings.Contains(spec.Binary, "/") {
				binaryInImage = path.Base(spec.Binary)
			}

			dockerArgs := []string{
				"run", "-d", "--rm",
				"--name", containerName,
				"--hostname", target.Hostname,
				"--network", spec.DockerNetwork,
				"-p", fmt.Sprintf("%d:26657", target.HostRPCPort),
				"-p", fmt.Sprintf("%d:8545", target.HostEVMRPCPort),
			}
			if !usePatchedImageLayout {
				dockerArgs = append(dockerArgs, "-v", fmt.Sprintf("%s:/data", target.Home))
			}
			dockerArgs = append(dockerArgs,
				dockerImage,
				"/bin/"+binaryInImage,
				"start", "--home", containerHome,
			)
			if strings.TrimSpace(spec.ChainID) != "" {
				dockerArgs = append(dockerArgs, "--chain-id", spec.ChainID)
			}
			dockerArgs = append(dockerArgs, spec.StartArgs...)
			if _, err := chainCmd(ctx, "docker", nil, dockerArgs...); err != nil {
				return messages.NodeRunResult{}, fmt.Errorf("start node docker container: %w", err)
			}
			defer func() {
				if containerName != "" {
					_, _ = chainCmd(context.Background(), "docker", nil, "rm", "-f", containerName)
				}
			}()
		} else {
			args := []string{"start", "--home", target.Home}
			if strings.TrimSpace(spec.ChainID) != "" {
				args = append(args, "--chain-id", spec.ChainID)
			}
			args = append(args, spec.StartArgs...)
			proc = exec.CommandContext(ctx, spec.Binary, args...)
			logFile, err := os.OpenFile(filepath.Join(target.Home, "node.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				return messages.NodeRunResult{}, fmt.Errorf("open node log file: %w", err)
			}
			defer logFile.Close()
			proc.Stdout = logFile
			proc.Stderr = logFile
			if err := proc.Start(); err != nil {
				return messages.NodeRunResult{}, fmt.Errorf("start node process: %w", err)
			}
			defer func() {
				if proc.Process != nil {
					_ = proc.Process.Kill()
					_, _ = proc.Process.Wait()
				}
			}()
		}
	}

	host := "127.0.0.1"
	rpcPort := spec.RPCPort
	evmPort := spec.EVMRPCPort
	if spec.RunnerType == "docker" {
		rpcPort = target.HostRPCPort
		evmPort = target.HostEVMRPCPort
	}

	if err := bench.WaitForPort(ctx, host, rpcPort, 2*time.Minute); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("wait tendermint rpc: %w", err)
	}
	if err := bench.WaitForPort(ctx, host, evmPort, 2*time.Minute); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("wait evm rpc: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	rpcURL := fmt.Sprintf("http://127.0.0.1:%d", evmPort)

	return doRun(ctx, client, spec, target, rpcURL, txs)
}

func doRun(
	ctx context.Context,
	client *http.Client,
	spec messages.BenchmarkSpec,
	target messages.NodeTarget,
	rpcURL string,
	txs []string,
) (messages.NodeRunResult, error) {
	if err := bench.WaitForHeight(ctx, client, rpcURL, spec.MinReadyHeight, 2*time.Minute); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("wait ready height: %w", err)
	}

	bench.BroadcastRawTxs(ctx, client, rpcURL, txs, 128)

	if err := bench.DetectIdleOrHalt(
		ctx,
		client,
		rpcURL,
		spec.NumIdle,
		time.Duration(spec.IdlePollIntervalSeconds)*time.Second,
		time.Duration(spec.ChainHaltIntervalSeconds)*time.Second,
	); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("wait idle/halt: %w", err)
	}

	end, err := bench.CurrentHeight(ctx, client, rpcURL)
	if err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("final height: %w", err)
	}

	statsPath := filepath.Join(target.Home, "block_stats.log")
	statsFile, err := os.Create(statsPath)
	if err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("create stats log: %w", err)
	}
	defer statsFile.Close()

	topTPS, err := bench.DumpBlockStats(ctx, statsFile, client, rpcURL, 2, end)
	if err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("dump block stats: %w", err)
	}

	if spec.OutDir != "" {
		copyPath := filepath.Join(spec.OutDir, fmt.Sprintf("node_%d_block_stats.log", target.GlobalSeq))
		if copyErr := copyFile(statsPath, copyPath); copyErr != nil {
			return messages.NodeRunResult{}, copyErr
		}
	}

	return messages.NodeRunResult{
		GlobalSeq: target.GlobalSeq,
		TxsSent:   len(txs),
		TopTPS:    topTPS,
		StatsFile: statsPath,
	}, nil
}

func writeJSON(path string, value interface{}) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json for %s: %w", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, out interface{}) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	if err := os.WriteFile(dst, in, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}

func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create destination dir %s: %w", dst, err)
	}

	return filepath.WalkDir(src, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, current)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		in, err := os.Open(current)
		if err != nil {
			return err
		}
		defer in.Close()

		info, err := d.Info()
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		defer out.Close()

		if _, err := io.Copy(out, in); err != nil {
			return err
		}
		return nil
	})
}

func bootstrapNodesAndGenesis(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	if spec.Binary == "" {
		return fmt.Errorf("benchmark.binary is required for bootstrap")
	}

	gentxPaths := make([]string, 0, spec.Validators)
	for _, node := range nodes {
		if err := os.RemoveAll(node.Home); err != nil {
			return fmt.Errorf("reset node home %s: %w", node.Home, err)
		}
		if err := os.MkdirAll(node.Home, 0o755); err != nil {
			return fmt.Errorf("recreate node home %s: %w", node.Home, err)
		}

		nodeName := fmt.Sprintf("%s-%d", node.Group, node.GroupSeq)
		initArgs := []string{"init", nodeName, "--home", node.Home, "--chain-id", spec.ChainID}
		if spec.Denom != "" {
			initArgs = append(initArgs, "--default-denom", spec.Denom)
		}
		if _, err := chainCmd(ctx, spec.Binary, nil, initArgs...); err != nil {
			return fmt.Errorf("init node %d: %w", node.GlobalSeq, err)
		}

		validatorKey, err := deterministicKey(node.GlobalSeq, 0)
		if err != nil {
			return err
		}
		if _, err := chainCmd(ctx, spec.Binary, []byte("00000000\n"),
			"keys", "unsafe-import-eth-key", validatorKeyName, hex.EncodeToString(crypto.FromECDSA(validatorKey)),
			"--home", node.Home,
			"--chain-id", spec.ChainID,
			"--keyring-backend", "test",
		); err != nil {
			return fmt.Errorf("import validator key for node %d: %w", node.GlobalSeq, err)
		}

		if node.Group == "validators" {
			if _, err := chainCmd(ctx, spec.Binary, nil,
				"genesis", "add-genesis-account", validatorKeyName, validatorInitialCoins+spec.Denom,
				"--home", node.Home,
				"--keyring-backend", "test",
			); err != nil {
				return fmt.Errorf("add validator self account for node %d: %w", node.GlobalSeq, err)
			}

			tmpGentx, err := os.CreateTemp("", fmt.Sprintf("gentx-%d-*.json", node.GlobalSeq))
			if err != nil {
				return fmt.Errorf("create temporary gentx file for node %d: %w", node.GlobalSeq, err)
			}
			gentxPath := tmpGentx.Name()
			if err := tmpGentx.Close(); err != nil {
				return fmt.Errorf("close temporary gentx file for node %d: %w", node.GlobalSeq, err)
			}
			if err := os.Remove(gentxPath); err != nil {
				return fmt.Errorf("prepare gentx output path for node %d: %w", node.GlobalSeq, err)
			}
			if _, err := chainCmd(ctx, spec.Binary, nil,
				"genesis", "gentx", validatorKeyName, validatorStakedCoins+spec.Denom,
				"--home", node.Home,
				"--chain-id", spec.ChainID,
				"--keyring-backend", "test",
				"--output-document", gentxPath,
				"--min-self-delegation", "1",
			); err != nil {
				return fmt.Errorf("gentx for validator node %d: %w", node.GlobalSeq, err)
			}
			gentxPaths = append(gentxPaths, gentxPath)
		}
	}

	leaderHome := ""
	for _, n := range nodes {
		if n.Group == "validators" && n.GroupSeq == 0 {
			leaderHome = n.Home
			break
		}
	}
	if leaderHome == "" {
		return fmt.Errorf("validator leader not found")
	}

	for _, node := range nodes {
		_ = node
	}

	type bulkCoin struct {
		Amount string `json:"amount"`
		Denom  string `json:"denom"`
	}
	type bulkAccount struct {
		Address string     `json:"address"`
		Coins   []bulkCoin `json:"coins"`
	}

	accounts := make([]bulkAccount, 0, len(nodes)*(spec.NumAccounts+1))
	for _, node := range nodes {
		if node.GlobalSeq != 0 {
			addr, err := bech32Address(node.GlobalSeq, 0, spec.AddressPrefix)
			if err != nil {
				return err
			}
			accounts = append(accounts, bulkAccount{
				Address: addr,
				Coins: []bulkCoin{{
					Amount: validatorInitialCoins,
					Denom:  spec.Denom,
				}},
			})
		}

		for i := 1; i <= spec.NumAccounts; i++ {
			acct, err := bech32Address(node.GlobalSeq, i, spec.AddressPrefix)
			if err != nil {
				return err
			}
			accounts = append(accounts, bulkAccount{
				Address: acct,
				Coins: []bulkCoin{{
					Amount: accountInitialCoins,
					Denom:  spec.Denom,
				}},
			})
		}
	}

	bulkTmp, err := os.CreateTemp("", "bulk-genesis-accounts-*.json")
	if err != nil {
		return fmt.Errorf("create temporary bulk genesis accounts file: %w", err)
	}
	bulkPath := bulkTmp.Name()
	if err := json.NewEncoder(bulkTmp).Encode(accounts); err != nil {
		_ = bulkTmp.Close()
		_ = os.Remove(bulkPath)
		return fmt.Errorf("encode bulk genesis accounts: %w", err)
	}
	if err := bulkTmp.Close(); err != nil {
		_ = os.Remove(bulkPath)
		return fmt.Errorf("close temporary bulk genesis accounts file: %w", err)
	}
	defer os.Remove(bulkPath)
	leaderGenesis := filepath.Join(leaderHome, "config", "genesis.json")

	if _, err := chainCmd(ctx, spec.Binary, nil,
		"genesis", "bulk-add-genesis-account", bulkPath,
		"--home", leaderHome,
	); err != nil {
		return fmt.Errorf("bulk add genesis accounts: %w", err)
	}

	gentxDir, err := os.MkdirTemp("", "evm-benchmark-gentx-")
	if err != nil {
		return fmt.Errorf("create gentx dir: %w", err)
	}
	defer os.RemoveAll(gentxDir)

	for i, p := range gentxPaths {
		content, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read gentx %s: %w", p, err)
		}
		dst := filepath.Join(gentxDir, fmt.Sprintf("gentx-%d.json", i))
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return fmt.Errorf("write gentx %s: %w", dst, err)
		}
	}

	if _, err := chainCmd(ctx, spec.Binary, nil,
		"genesis", "collect-gentxs",
		"--home", leaderHome,
		"--gentx-dir", gentxDir,
	); err != nil {
		return fmt.Errorf("collect gentxs: %w", err)
	}

	if _, err := chainCmd(ctx, spec.Binary, nil, "genesis", "validate", "--home", leaderHome); err != nil {
		return fmt.Errorf("validate genesis: %w", err)
	}

	genesisBz, err := os.ReadFile(leaderGenesis)
	if err != nil {
		return fmt.Errorf("read leader genesis: %w", err)
	}

	genesisBz, err = patchGenesisContractAuthAccount(genesisBz, spec.AddressPrefix)
	if err != nil {
		return err
	}
	genesisBz, err = applyGenesisPatch(genesisBz, spec.GenesisPatch)
	if err != nil {
		return err
	}
	if err := os.WriteFile(leaderGenesis, genesisBz, 0o644); err != nil {
		return fmt.Errorf("write leader genesis: %w", err)
	}

	for _, node := range nodes {
		targetGenesis := filepath.Join(node.Home, "config", "genesis.json")
		if err := os.WriteFile(targetGenesis, genesisBz, 0o644); err != nil {
			return fmt.Errorf("write node genesis %s: %w", targetGenesis, err)
		}
		if err := patchConfigToml(node.Home, nodes, spec); err != nil {
			return err
		}
	}

	return nil
}

func patchConfigToml(home string, nodes []messages.NodeTarget, spec messages.BenchmarkSpec) error {
	configPath := filepath.Join(home, "config", "config.toml")
	bz, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read config.toml: %w", err)
	}

	peerEntries := make([]string, 0, len(nodes)-1)
	for _, n := range nodes {
		nodeIDBz, err := chainCmd(context.Background(), spec.Binary, nil, "comet", "show-node-id", "--home", n.Home)
		if err != nil {
			return fmt.Errorf("read node id for %s: %w", n.Home, err)
		}
		nodeID := strings.TrimSpace(string(nodeIDBz))
		if n.Home == home {
			continue
		}
		peerEntries = append(peerEntries, fmt.Sprintf("%s@%s:26656", nodeID, n.Hostname))
	}

	var cfg map[string]interface{}
	if err := tomlv2.Unmarshal(bz, &cfg); err != nil {
		return fmt.Errorf("decode config.toml: %w", err)
	}

	setNested(cfg, []string{"p2p", "persistent_peers"}, strings.Join(peerEntries, ","))
	setNested(cfg, []string{"mempool", "recheck"}, false)
	setNested(cfg, []string{"mempool", "size"}, int64(10000))
	setNested(cfg, []string{"consensus", "timeout_commit"}, "1s")
	mergeMaps(cfg, spec.ConfigPatch)

	outCfg, err := tomlv2.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode config.toml: %w", err)
	}
	if err := os.WriteFile(configPath, outCfg, 0o644); err != nil {
		return fmt.Errorf("write config.toml: %w", err)
	}

	appPath := filepath.Join(home, "config", "app.toml")
	appBz, err := os.ReadFile(appPath)
	if err != nil {
		return fmt.Errorf("read app.toml: %w", err)
	}
	var appCfg map[string]interface{}
	if err := tomlv2.Unmarshal(appBz, &appCfg); err != nil {
		return fmt.Errorf("decode app.toml: %w", err)
	}
	setNested(appCfg, []string{"minimum-gas-prices"}, fmt.Sprintf("0%s", spec.Denom))
	setNested(appCfg, []string{"json-rpc", "enable"}, true)
	mergeMaps(appCfg, spec.AppPatch)

	outApp, err := tomlv2.Marshal(appCfg)
	if err != nil {
		return fmt.Errorf("encode app.toml: %w", err)
	}
	if err := os.WriteFile(appPath, outApp, 0o644); err != nil {
		return fmt.Errorf("write app.toml: %w", err)
	}

	clientPath := filepath.Join(home, "config", "client.toml")
	clientBz, err := os.ReadFile(clientPath)
	if err != nil {
		return fmt.Errorf("read client.toml: %w", err)
	}
	var clientCfg map[string]interface{}
	if err := tomlv2.Unmarshal(clientBz, &clientCfg); err != nil {
		return fmt.Errorf("decode client.toml: %w", err)
	}
	if strings.TrimSpace(spec.ChainID) != "" {
		setNested(clientCfg, []string{"chain-id"}, spec.ChainID)
	}
	outClient, err := tomlv2.Marshal(clientCfg)
	if err != nil {
		return fmt.Errorf("encode client.toml: %w", err)
	}
	if err := os.WriteFile(clientPath, outClient, 0o644); err != nil {
		return fmt.Errorf("write client.toml: %w", err)
	}

	return nil
}

func setNested(m map[string]interface{}, keys []string, val interface{}) {
	if len(keys) == 0 {
		return
	}
	cur := m
	for i := 0; i < len(keys)-1; i++ {
		k := keys[i]
		next, ok := cur[k]
		if !ok {
			nm := map[string]interface{}{}
			cur[k] = nm
			cur = nm
			continue
		}
		nm, ok := next.(map[string]interface{})
		if !ok {
			nm = map[string]interface{}{}
			cur[k] = nm
		}
		cur = nm
	}
	cur[keys[len(keys)-1]] = val
}

func mergeMaps(dst, patch map[string]interface{}) {
	if patch == nil {
		return
	}
	for k, v := range patch {
		v = normalizeValue(v)
		if vMap, ok := v.(map[string]interface{}); ok {
			if existing, ok := dst[k].(map[string]interface{}); ok {
				mergeMaps(existing, vMap)
				dst[k] = existing
				continue
			}
			nm := map[string]interface{}{}
			mergeMaps(nm, vMap)
			dst[k] = nm
			continue
		}
		dst[k] = v
	}
}

func normalizeValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, vv := range t {
			out[k] = normalizeValue(vv)
		}
		return out
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, vv := range t {
			out[fmt.Sprintf("%v", k)] = normalizeValue(vv)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, item := range t {
			out[i] = normalizeValue(item)
		}
		return out
	default:
		return v
	}
}

func applyGenesisPatch(genesis []byte, patch map[string]interface{}) ([]byte, error) {
	if len(patch) == 0 {
		return genesis, nil
	}
	var doc map[string]interface{}
	if err := json.Unmarshal(genesis, &doc); err != nil {
		return nil, fmt.Errorf("decode genesis json: %w", err)
	}
	normalizedPatch, ok := normalizeValue(patch).(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("invalid genesis patch structure")
	}
	mergeMaps(doc, normalizedPatch)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode genesis json: %w", err)
	}
	return out, nil
}

func chainCmd(ctx context.Context, binary string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %s failed: %w\n%s", binary, strings.Join(args, " "), err, string(out))
	}
	return out, nil
}

func deterministicKey(globalSeq, index int) (*ecdsa.PrivateKey, error) {
	var raw [32]byte
	seed := (uint64(globalSeq+1) << 32) | uint64(index)
	binary.BigEndian.PutUint64(raw[24:], seed)
	return crypto.ToECDSA(raw[:])
}

func bech32Address(globalSeq, index int, prefix string) (string, error) {
	key, err := deterministicKey(globalSeq, index)
	if err != nil {
		return "", err
	}
	ethAddr := crypto.PubkeyToAddress(key.PublicKey)
	fiveBit, err := bech32.ConvertBits(ethAddr.Bytes(), 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("convert bits: %w", err)
	}
	return bech32.Encode(prefix, fiveBit)
}

func patchGenesisContractAuthAccount(genesis []byte, prefix string) ([]byte, error) {
	var doc map[string]interface{}
	if err := json.Unmarshal(genesis, &doc); err != nil {
		return nil, fmt.Errorf("decode genesis json: %w", err)
	}

	appState, ok := doc["app_state"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("genesis missing app_state")
	}
	authState, ok := appState["auth"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("genesis missing app_state.auth")
	}

	accounts, _ := authState["accounts"].([]interface{})
	contractAddr, err := ethAddressToBech32(common.HexToAddress(erc20ContractAddress), prefix)
	if err != nil {
		return nil, err
	}

	for _, entry := range accounts {
		m, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if addr, ok := m["address"].(string); ok && addr == contractAddr {
			out, err := json.Marshal(doc)
			if err != nil {
				return nil, fmt.Errorf("encode genesis json: %w", err)
			}
			return out, nil
		}
	}

	authState["accounts"] = append(accounts, map[string]interface{}{
		"@type":    "/cosmos.auth.v1beta1.BaseAccount",
		"address":  contractAddr,
		"sequence": "1",
	})

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode genesis json: %w", err)
	}
	return out, nil
}

func ethAddressToBech32(addr common.Address, prefix string) (string, error) {
	fiveBit, err := bech32.ConvertBits(addr.Bytes(), 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("convert bits: %w", err)
	}
	return bech32.Encode(prefix, fiveBit)
}
