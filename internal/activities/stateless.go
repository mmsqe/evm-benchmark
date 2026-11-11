package activities

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
	tomlv2 "github.com/pelletier/go-toml/v2"
	"go.temporal.io/sdk/activity"
)

type Activity struct{}

const (
	validatorKeyName      = "validator"
	validatorInitialCoins = "100000000000000000000"
	validatorStakedCoins  = "10000000000000000000"
	accountInitialCoins   = "10000000000000000000000000"
	erc20ContractAddress  = "0x1000000000000000000000000000000000000000"
)

func (a *Activity) GenerateLayout(ctx context.Context, req messages.LayoutRequest) (messages.LayoutResponse, error) {
	spec := req.Spec
	chain, err := resolveRuntime(spec)
	if err != nil {
		return messages.LayoutResponse{}, err
	}
	if err := chain.Validate(spec); err != nil {
		return messages.LayoutResponse{}, err
	}
	if err := chain.EnrichSpec(&spec); err != nil {
		return messages.LayoutResponse{}, err
	}

	if err := os.MkdirAll(spec.DataDir, 0o755); err != nil {
		return messages.LayoutResponse{}, fmt.Errorf("create data dir: %w", err)
	}
	if spec.OutDir != "" {
		if err := os.MkdirAll(spec.OutDir, 0o755); err != nil {
			return messages.LayoutResponse{}, fmt.Errorf("create output dir: %w", err)
		}
	}

	nodes := make([]messages.NodeTarget, spec.Validators+spec.Fullnodes)
	for i := range nodes {
		group, groupSeq := "validators", i
		if i >= spec.Validators {
			group, groupSeq = "fullnodes", i-spec.Validators
		}
		nodes[i] = messages.NodeTarget{
			GlobalSeq:      i,
			Group:          group,
			GroupSeq:       groupSeq,
			Hostname:       fmt.Sprintf(spec.HostnameTemplate, i),
			Home:           filepath.Join(spec.DataDir, group, strconv.Itoa(groupSeq)),
			HostRPCPort:    spec.RPCPort + i,
			HostEVMRPCPort: spec.EVMRPCPort + i,
		}
		if err := os.MkdirAll(nodes[i].Home, 0o755); err != nil {
			return messages.LayoutResponse{}, fmt.Errorf("create node home: %w", err)
		}
	}

	if err := chain.Bootstrap(ctx, spec, nodes); err != nil {
		return messages.LayoutResponse{}, err
	}

	if err := writeJSON(filepath.Join(spec.DataDir, "config.json"), spec); err != nil {
		return messages.LayoutResponse{}, err
	}
	if err := writeJSON(filepath.Join(spec.DataDir, "nodes.json"), nodes); err != nil {
		return messages.LayoutResponse{}, err
	}
	return messages.LayoutResponse{Nodes: nodes}, nil
}

func (a *Activity) LoadLayout(_ context.Context, req messages.LayoutRequest) (messages.LayoutResponse, error) {
	if req.Spec.OutDir != "" {
		if err := os.MkdirAll(req.Spec.OutDir, 0o755); err != nil {
			return messages.LayoutResponse{}, fmt.Errorf("create output dir: %w", err)
		}
	}

	nodesPath := filepath.Join(req.Spec.DataDir, "nodes.json")
	var nodes []messages.NodeTarget
	if err := readJSON(nodesPath, &nodes); err != nil {
		return messages.LayoutResponse{}, fmt.Errorf("load existing node layout %s: %w", nodesPath, err)
	}
	if len(nodes) == 0 {
		return messages.LayoutResponse{}, fmt.Errorf("existing node layout %s is empty", nodesPath)
	}

	for _, n := range nodes {
		configPath := filepath.Join(n.Home, "config", "config.toml")
		if _, err := os.Stat(configPath); err != nil {
			continue
		}
		if err := patchConfigToml(n.Home, nodes, req.Spec); err != nil {
			return messages.LayoutResponse{}, fmt.Errorf("refresh node config for %s: %w", n.Home, err)
		}
	}

	return messages.LayoutResponse{Nodes: nodes}, nil
}

func (a *Activity) GenerateTxs(ctx context.Context, req messages.GenerateTxsRequest) (int, error) {
	logger := activity.GetLogger(ctx)
	logger.Info("generate txs started", "node", req.Target.GlobalSeq, "accounts", req.Spec.NumAccounts, "txs_per_account", req.Spec.NumTxs)

	chain, err := resolveRuntime(req.Spec)
	if err != nil {
		return 0, err
	}
	if err := chain.EnrichSpec(&req.Spec); err != nil {
		return 0, err
	}

	txDir := filepath.Join(req.Spec.DataDir, "txs")
	if err := os.MkdirAll(txDir, 0o755); err != nil {
		return 0, fmt.Errorf("create tx dir: %w", err)
	}
	txPath := filepath.Join(txDir, fmt.Sprintf("%d.json", req.Target.GlobalSeq))

	// A family may generate its own transactions (Tempo's native 0x76
	// envelope); otherwise fall through to the shared legacy signer.
	if producer, ok := chain.(txProducer); ok && producer.ProducesTxs(req.Spec) {
		count, prodErr := producer.ProduceTxs(ctx, req.Spec, req.Target, txPath)
		if prodErr != nil {
			return 0, prodErr
		}
		logger.Info("generate txs completed via chain producer", "node", req.Target.GlobalSeq, "total_txs", count)
		return count, nil
	}

	txFile, err := os.Create(txPath)
	if err != nil {
		return 0, fmt.Errorf("create tx file: %w", err)
	}
	defer txFile.Close()

	writer := bufio.NewWriterSize(txFile, 1<<20) // write errors surface at the flush
	writer.WriteString("[")
	separator := ""

	lastHeartbeat := time.Now()
	lastProgressLog := time.Now()
	progressStep := max(req.Spec.NumAccounts/20, 1)
	txCount, err := bench.GenerateSignedTxsStream(req.Spec, req.Target.GlobalSeq, func(done, total int) {
		if done == total || done == 1 || time.Since(lastHeartbeat) >= 15*time.Second {
			activity.RecordHeartbeat(ctx, map[string]int{"accounts_done": done, "accounts_total": total})
			lastHeartbeat = time.Now()
		}
		if done == total || done == 1 || done%progressStep == 0 || time.Since(lastProgressLog) >= 5*time.Second {
			percent := (done * 100) / total
			logger.Info("generate txs progress", "node", req.Target.GlobalSeq, "accounts_done", done, "accounts_total", total, "percent", percent)
			lastProgressLog = time.Now()
		}
	}, func(raw string) error {
		_, err := fmt.Fprintf(writer, "%s%q", separator, raw) // hex needs no JSON escaping
		separator = ","
		return err
	})
	if err != nil {
		return 0, err
	}

	writer.WriteString("]")
	if err := writer.Flush(); err != nil {
		return 0, fmt.Errorf("flush tx file: %w", err)
	}
	logger.Info("generate txs completed", "node", req.Target.GlobalSeq, "total_txs", txCount)
	return txCount, nil
}

func (a *Activity) PatchImage(ctx context.Context, req messages.PatchImageRequest) (messages.PatchImageResponse, error) {
	spec := req.Spec

	fromImage := strings.TrimSpace(spec.PatchImage.FromImage)
	if fromImage == "" {
		fromImage = strings.TrimSpace(spec.DockerImage)
	}
	if fromImage == "" {
		return messages.PatchImageResponse{}, fmt.Errorf("patchimage requires patch_image_from_image or docker_image")
	}

	toImage := strings.TrimSpace(spec.PatchImage.ToImage)
	if toImage == "" {
		toImage = fromImage + "-patched"
	}

	sourceDir := strings.TrimSpace(spec.PatchImage.SourceDir)
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

	dst := strings.TrimSpace(spec.PatchImage.Dest)
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
	chain, err := resolveRuntime(spec)
	if err != nil {
		return messages.NodeRunResult{}, err
	}
	if err := chain.EnrichSpec(&spec); err != nil {
		return messages.NodeRunResult{}, err
	}
	spec.BroadcastConcurrency = max(spec.BroadcastConcurrency, 1)
	target := req.Target
	if target.Group == "validators" && !spec.ValidatorGenerateLoad {
		return messages.NodeRunResult{GlobalSeq: target.GlobalSeq}, nil
	}

	txPath := filepath.Join(spec.DataDir, "txs", fmt.Sprintf("%d.json", target.GlobalSeq))
	var txs []string
	if err := readJSON(txPath, &txs); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("load node txs: %w", err)
	}

	if err := chain.Validate(spec); err != nil {
		return messages.NodeRunResult{}, err
	}
	if err := chain.PreStartCheck(spec, target); err != nil {
		return messages.NodeRunResult{}, err
	}
	containerName := ""
	if spec.StartNode {
		var stop func()
		if spec.RunnerType == "docker" {
			containerName, stop, err = startDockerNode(ctx, spec, target, req.DockerImageOverride)
		} else {
			argv, dir := chain.LocalStartCommand(spec, target)
			ctx, stop, err = startLocalNode(ctx, argv, dir, filepath.Join(cmp.Or(dir, target.Home), "node.log"))
		}
		if err != nil {
			return messages.NodeRunResult{}, err
		}
		defer stop()
	}

	// waitPort gives up on a port with the container's log or the node's exit
	// as the cause.
	waitPort := func(port int, what, logName string) error {
		err := bench.WaitForPort(ctx, "127.0.0.1", port, 2*time.Minute)
		if err == nil {
			return nil
		}
		if spec.RunnerType == "docker" {
			if logPath, dumpErr := dumpDockerLogs(spec, target, containerName, logName); dumpErr == nil {
				return fmt.Errorf("wait %s: %w (docker logs: %s)", what, err, logPath)
			}
		}
		return withCause(ctx, fmt.Errorf("wait %s: %w", what, err))
	}
	if chain.HasConsensusRPC() {
		rpcPort := spec.RPCPort
		if spec.RunnerType == "docker" {
			rpcPort = target.HostRPCPort
		}
		if err := waitPort(rpcPort, "consensus rpc", "startup-rpc"); err != nil {
			return messages.NodeRunResult{}, err
		}
	}
	evmPort := chain.EVMRPCPort(spec, target.GlobalSeq)
	// A remote endpoint has no local port; doRun's ready-height check covers it.
	if spec.RemoteRPCURL == "" {
		if err := waitPort(evmPort, "evm rpc", "startup-evm-rpc"); err != nil {
			return messages.NodeRunResult{}, err
		}
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		MaxConnsPerHost:       spec.BroadcastConcurrency,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Timeout: time.Duration(spec.BroadcastRequestTimeoutSeconds) * time.Second, Transport: transport}
	defer transport.CloseIdleConnections()
	rpcURL := fmt.Sprintf("http://127.0.0.1:%d", evmPort)
	if spec.RemoteRPCURL != "" {
		rpcURL = spec.RemoteRPCURL
	}

	res, err := doRun(ctx, client, spec, target, rpcURL, txs)
	if reporter, ok := chain.(metricsReporter); ok && err == nil && spec.StartNode {
		err = reportNodeMetrics(ctx, client, reporter, spec, target, res.StatsFile)
	}
	return res, withCause(ctx, err)
}

// startLocalNode launches the node with its output appended to logPath. The
// returned context ends when the node exits, with the end of its log as the
// cause; stop terminates the node and waits for it.
func startLocalNode(ctx context.Context, argv []string, dir, logPath string) (context.Context, func(), error) {
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return ctx, nil, fmt.Errorf("open node log file: %w", err)
	}
	ctx, stopRun := context.WithCancelCause(ctx)
	proc := exec.CommandContext(ctx, argv[0], argv[1:]...)
	proc.Dir = dir
	proc.Stdout = logFile
	proc.Stderr = logFile
	// SIGTERM first so the node can flush; killed if still up after WaitDelay.
	proc.Cancel = func() error { return proc.Process.Signal(syscall.SIGTERM) }
	proc.WaitDelay = 5 * time.Second
	if err := proc.Start(); err != nil {
		stopRun(nil)
		logFile.Close()
		return ctx, nil, fmt.Errorf("start node process: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		err := proc.Wait()
		tail, _ := os.ReadFile(logPath)
		stopRun(fmt.Errorf("node process exited (%v); end of %s:\n%s", err, logPath, tail[max(0, len(tail)-2048):]))
		close(exited)
	}()
	return ctx, func() { stopRun(nil); <-exited; logFile.Close() }, nil
}

// withCause appends to err why ctx was canceled, such as the node's exit and the
// end of its log. A live context, or one canceled without a cause, adds nothing.
func withCause(ctx context.Context, err error) error {
	if cause := context.Cause(ctx); err != nil && cause != ctx.Err() {
		return fmt.Errorf("%w: %w", err, cause)
	}
	return err
}

func doRun(
	ctx context.Context,
	client *http.Client,
	spec messages.BenchmarkSpec,
	target messages.NodeTarget,
	rpcURL string,
	txs []string,
) (messages.NodeRunResult, error) {
	logger := activity.GetLogger(ctx)

	if err := bench.WaitForHeight(ctx, client, rpcURL, spec.MinReadyHeight, 2*time.Minute); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("wait ready height: %w", err)
	}

	if len(txs) > 0 {
		suggested, err := bench.SuggestedGasPrice(ctx, client, rpcURL)
		if err != nil {
			logger.Warn("dynamic gas price estimate failed", "node", target.GlobalSeq, "error", err, "fallback_gas_price_wei", spec.GasPriceWei)
		} else {
			// 20% over the suggestion, so bursty load is not refused as underpriced.
			effective := max(suggested*12/10, spec.GasPriceWei)
			switch {
			case effective == spec.GasPriceWei:
			case runtimeGeneratedTxs(spec):
				// The legacy signer would replace a native envelope with a legacy
				// one. Native txs carry an EIP-1559 style max fee, which absorbs
				// base-fee movement up to that max.
				logger.Warn(
					"suggested gas price exceeds the max fee these txs were signed with; they may be rejected as underpriced",
					"node", target.GlobalSeq,
					"suggested_gas_price_wei", effective,
					"signed_max_fee_per_gas", spec.GasPriceWei,
				)
			default:
				spec.GasPriceWei = effective
				regenerated, genErr := bench.GenerateSignedTxs(spec, target.GlobalSeq)
				if genErr != nil {
					return messages.NodeRunResult{}, fmt.Errorf("regenerate txs with dynamic gas price: %w", genErr)
				}
				txs = regenerated
			}

			logger.Info("using runtime gas price", "node", target.GlobalSeq, "suggested_gas_price_wei", suggested, "effective_gas_price_wei", spec.GasPriceWei)
		}
	}

	// A shared chain carries everyone's transactions: count only ours, by
	// hash, from the block the run started at. Remote runs are Tempo only.
	shared := spec.RemoteRPCURL != ""
	statsFrom := int64(2)
	var own bench.TxSet
	if shared {
		head, err := bench.CurrentHeight(ctx, client, rpcURL)
		if err != nil {
			return messages.NodeRunResult{}, fmt.Errorf("start height: %w", err)
		}
		statsFrom = head + 1
		own = bench.NewTxSet(txs, tempotx.Hash)
	}

	opts := bench.BroadcastOptions{
		Concurrency: spec.BroadcastConcurrency,
		BatchSize:   spec.BroadcastBatchSize,
		Watermark:   spec.BroadcastPendingWatermark,
	}
	var sendStats bench.BroadcastStats
	if shared {
		// A node that is not the block producer must see each account's
		// nonces in order (see BroadcastStreams). The file holds each
		// account's transactions contiguously.
		accounts := make([][]string, 0, spec.NumAccounts)
		for start := 0; start < len(txs); start += spec.NumTxs {
			accounts = append(accounts, txs[start:min(start+spec.NumTxs, len(txs))])
		}
		sendStats = bench.BroadcastStreams(ctx, client, rpcURL, accounts, opts)
	} else {
		sendStats = bench.BroadcastRawTxs(ctx, client, rpcURL, txs, opts)
	}
	sendEnd := time.Now()
	logger.Info("broadcast complete",
		"node", target.GlobalSeq,
		"seconds", sendStats.Duration.Seconds(),
		"send_rate", sendStats.Rate(),
		"max_pending", sendStats.MaxPending,
		"rejected", sendStats.Rejected,
		"failed", sendStats.Failed,
		"req_p50", sendStats.ReqP50,
	)

	pollInterval := time.Duration(spec.IdlePollIntervalSeconds) * time.Second
	haltAfter := time.Duration(spec.ChainHaltIntervalSeconds) * time.Second
	if shared {
		if err := bench.WaitForTxs(ctx, client, rpcURL, own, statsFrom, spec.NumIdle, pollInterval, haltAfter); err != nil {
			return messages.NodeRunResult{}, fmt.Errorf("wait for own txs: %w", err)
		}
	} else if err := bench.DetectIdleOrHalt(ctx, client, rpcURL, spec.NumIdle, pollInterval, haltAfter); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("wait idle/halt: %w", err)
	}

	pendingTxpool := int64(-1) // unknown, unless the pool answers
	if pending, queued, err := bench.TxPoolStatus(ctx, client, rpcURL); err == nil {
		pendingTxpool = pending + queued
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
	w := bufio.NewWriter(statsFile) // writes are checked once, at the flush

	writeSendSummary(w, sendStats, spec.BroadcastRequestTimeoutSeconds)
	if sendStats.Undersaturated() {
		logger.Warn("chain kept pace with the sender; rate is a lower bound",
			"node", target.GlobalSeq, "max_pending", sendStats.MaxPending, "sent", sendStats.Sent)
	}

	stats, err := bench.DumpBlockStats(ctx, w, client, rpcURL, statsFrom, end, len(txs), sendEnd, own)
	// Flushed either way: a dump cut short still leaves the send summary.
	if err := errors.Join(err, w.Flush()); err != nil {
		return messages.NodeRunResult{}, fmt.Errorf("dump block stats: %w", err)
	}
	if stats.TooShort() {
		logger.Warn(
			"load drained too fast to measure a rate; raise num_accounts*num_txs",
			"node", target.GlobalSeq,
			"active_seconds", stats.ActiveSeconds,
			"full_seconds", stats.FullSeconds,
			"txs_sent", len(txs),
		)
	}

	topTPS := make([]float64, 0, len(stats.Peaks))
	for _, p := range stats.Peaks {
		topTPS = append(topTPS, p.TPS)
	}

	if spec.OutDir != "" {
		copyPath := filepath.Join(spec.OutDir, fmt.Sprintf("node_%d_block_stats.log", target.GlobalSeq))
		if copyErr := copyFile(statsPath, copyPath); copyErr != nil {
			return messages.NodeRunResult{}, copyErr
		}
	}

	return messages.NodeRunResult{
		GlobalSeq:     target.GlobalSeq,
		TxsSent:       len(txs),
		IncludedTxs:   stats.IncludedTxs,
		SustainedTPS:  stats.SustainedTPS,
		FullSecondTPS: stats.FullSecondTPS,
		ActiveSeconds: stats.ActiveSeconds,
		FullSeconds:   stats.FullSeconds,
		DrainTPS:      stats.DrainTPS,
		DrainSeconds:  stats.DrainSeconds,
		PendingTxpool: pendingTxpool,
		TopTPS:        topTPS,
		StatsFile:     statsPath,
	}, nil
}

// writeSendSummary writes how the send went, ahead of the block stats so the two
// rates sit together in the file: an inclusion rate at or below the send rate,
// with the pool never deep, means the run measured submission rather than
// execution.
func writeSendSummary(w io.Writer, sendStats bench.BroadcastStats, requestTimeoutSeconds int) {
	fmt.Fprintf(w,
		"send_summary sent=%d seconds=%.2f rate=%.0f max_pending=%d rejected=%d failed=%d req_p50=%.2fs req_p95=%.2fs req_max=%.2fs\n",
		sendStats.Sent, sendStats.Duration.Seconds(), sendStats.Rate(), sendStats.MaxPending,
		sendStats.Rejected, sendStats.Failed, sendStats.ReqP50.Seconds(), sendStats.ReqP95.Seconds(),
		sendStats.ReqMax.Seconds())
	if sendStats.Failed > 0 {
		fmt.Fprintf(w,
			"send_warning %d transactions were in requests the node never answered (broadcast_request_timeout_seconds=%d); "+
				"those it admitted anyway are counted below, the rest are missing.\n",
			sendStats.Failed, requestTimeoutSeconds)
	}
	reasons := slices.Collect(maps.Keys(sendStats.RejectReasons))
	slices.SortFunc(reasons, func(a, b string) int { return sendStats.RejectReasons[b] - sendStats.RejectReasons[a] })
	for _, msg := range reasons {
		fmt.Fprintf(w, "send_reject count=%d reason=%q\n", sendStats.RejectReasons[msg], msg)
	}
	if sendStats.PoolStalled {
		fmt.Fprint(w,
			"send_warning the pool stopped draining above the watermark, so the rest was sent without waiting: "+
				"the node may be holding transactions it never forwards.\n")
	}
	if sendStats.PoolUnknown && sendStats.Sent > 0 {
		fmt.Fprint(w,
			"send_warning the pool was never read (no txpool_status, or too small a load to sample), so "+
				"whether the chain ever fell behind the sender is unknown: this rate is a lower bound.\n")
	}
	if sendStats.Undersaturated() {
		// Passing the whole-seconds check does not imply this: a run can span
		// plenty of seconds while the chain drains everything as it arrives.
		fmt.Fprintf(w,
			"send_warning the pool never held more than %d transactions (%.0f%% of the load): the chain kept "+
				"pace with the sender, so this rate is a lower bound on it, not a measurement. Raise the load, "+
				"or constrain the chain (block gas limit) so block production binds.\n",
			sendStats.MaxPending, 100*float64(sendStats.MaxPending)/float64(sendStats.Sent))
	}
}

// reportNodeMetrics saves the node's metrics as metrics.prom beside its stats
// log and appends their summary to the log, refreshing both copies in out_dir.
// RunNode calls it before stopping the node, so the scrape covers the run.
func reportNodeMetrics(
	ctx context.Context,
	client *http.Client,
	reporter metricsReporter,
	spec messages.BenchmarkSpec,
	target messages.NodeTarget,
	statsPath string,
) error {
	metricsURL := reporter.MetricsURL(spec, target.GlobalSeq)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metricsURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("scrape metrics %s: %w", metricsURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("scrape metrics %s: %s", metricsURL, resp.Status)
	}
	metrics, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("scrape metrics %s: %w", metricsURL, err)
	}
	lines, err := reporter.SummarizeMetrics(metrics)
	if err != nil {
		return err
	}
	for _, line := range lines {
		activity.GetLogger(ctx).Info(line, "node", target.GlobalSeq)
	}

	metricsPath := filepath.Join(filepath.Dir(statsPath), "metrics.prom")
	if err := os.WriteFile(metricsPath, metrics, 0o644); err != nil {
		return fmt.Errorf("write metrics: %w", err)
	}
	statsFile, err := os.OpenFile(statsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open stats log: %w", err)
	}
	_, err = statsFile.WriteString(strings.Join(lines, "\n") + "\n")
	if closeErr := statsFile.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("append metrics summary: %w", err)
	}

	if spec.OutDir == "" {
		return nil
	}
	for src, name := range map[string]string{statsPath: "block_stats.log", metricsPath: "metrics.prom"} {
		if err := copyFile(src, filepath.Join(spec.OutDir, fmt.Sprintf("node_%d_%s", target.GlobalSeq, name))); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json for %s: %w", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func readJSON(path string, out any) error {
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

// startDockerNode runs the node's container, returning its name and what stops
// it: removal, unless docker_keep_containers leaves it for `docker logs`.
func startDockerNode(ctx context.Context, spec messages.BenchmarkSpec, target messages.NodeTarget, imageOverride string) (string, func(), error) {
	if _, err := chainCmd(ctx, "docker", nil, "network", "create", spec.DockerNetwork); err != nil && !strings.Contains(err.Error(), "already exists") {
		return "", nil, fmt.Errorf("create docker network %q: %w", spec.DockerNetwork, err)
	}
	if err := ensureDockerVolumeHostDirs(spec.DockerVolumes); err != nil {
		return "", nil, fmt.Errorf("prepare docker volume host dirs: %w", err)
	}

	name := fmt.Sprintf("evm-benchmark-%d", target.GlobalSeq)
	containerHome := path.Join("/data", target.Group, strconv.Itoa(target.GroupSeq))
	// The node home is mounted from the host unless the layout is in the
	// image already: patched in, reused, or never generated here.
	_, statErr := os.Stat(filepath.Join(target.Home, "config", "genesis.json"))
	mountHome := imageOverride == "" && !spec.SkipGenerateLayout && statErr == nil

	args := []string{"run", "-d"}
	if !spec.DockerKeepContainers {
		args = append(args, "--rm")
	}
	args = append(args,
		"--name", name,
		"--hostname", target.Hostname,
		"--network", spec.DockerNetwork,
		"-p", fmt.Sprintf("%d:26657", target.HostRPCPort),
		"-p", fmt.Sprintf("%d:8545", target.HostEVMRPCPort),
	)
	if mountHome {
		args = append(args, "-v", fmt.Sprintf("%s:%s", target.Home, containerHome))
	}
	for _, volume := range spec.DockerVolumes {
		if volume = strings.TrimSpace(volume); volume != "" {
			args = append(args, "-v", volume)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(spec.DockerEnv)) {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, spec.DockerEnv[k]))
	}
	args = append(args,
		cmp.Or(imageOverride, spec.DockerImage),
		"/bin/"+path.Base(spec.Binary),
		"start", "--home", containerHome,
	)
	args = append(args, dockerDefaultBindArgs(spec.StartArgs)...)
	if strings.TrimSpace(spec.ChainID) != "" {
		args = append(args, "--chain-id", spec.ChainID)
	}
	args = append(args, spec.StartArgs...)
	if _, err := chainCmd(ctx, "docker", nil, args...); err != nil {
		return "", nil, fmt.Errorf("start node docker container: %w", err)
	}
	stop := func() {}
	if !spec.DockerKeepContainers {
		stop = func() { _, _ = chainCmd(context.Background(), "docker", nil, "rm", "-f", name) }
	}
	return name, stop, nil
}

func ensureDockerVolumeHostDirs(volumes []string) error {
	for _, volume := range volumes {
		volume = strings.TrimSpace(volume)
		if volume == "" {
			continue
		}

		parts := strings.Split(volume, ":")
		if len(parts) < 2 {
			continue // likely a named volume
		}

		hostPath := strings.TrimSpace(parts[0])
		if hostPath == "" || !strings.HasPrefix(hostPath, "/") {
			continue
		}

		if err := os.MkdirAll(hostPath, 0o755); err != nil {
			return fmt.Errorf("create host mount dir %s: %w", hostPath, err)
		}
	}
	return nil
}

func dockerDefaultBindArgs(startArgs []string) []string {
	args := make([]string, 0, 3)
	if !hasStartArgPrefix(startArgs, "--rpc.laddr") {
		args = append(args, "--rpc.laddr=tcp://0.0.0.0:26657")
	}
	if !hasStartArgPrefix(startArgs, "--json-rpc.address") {
		args = append(args, "--json-rpc.address=0.0.0.0:8545")
	}
	if !hasStartArgPrefix(startArgs, "--json-rpc.ws-address") {
		args = append(args, "--json-rpc.ws-address=0.0.0.0:8546")
	}
	return args
}

func hasStartArgPrefix(startArgs []string, prefix string) bool {
	return slices.ContainsFunc(startArgs, func(arg string) bool {
		arg = strings.TrimSpace(arg)
		return arg == prefix || strings.HasPrefix(arg, prefix+"=")
	})
}

func dumpDockerLogs(spec messages.BenchmarkSpec, target messages.NodeTarget, containerName, reason string) (string, error) {
	if strings.TrimSpace(containerName) == "" {
		return "", fmt.Errorf("container name is empty")
	}

	out, err := chainCmd(context.Background(), "docker", nil, "logs", containerName)
	if err != nil {
		out = []byte(err.Error())
	}

	logDir := strings.TrimSpace(spec.OutDir)
	if logDir == "" {
		logDir = filepath.Join(spec.DataDir, "docker-logs")
	}
	if mkErr := os.MkdirAll(logDir, 0o755); mkErr != nil {
		return "", fmt.Errorf("create docker log dir %s: %w", logDir, mkErr)
	}

	logPath := filepath.Join(logDir, fmt.Sprintf("node_%d_%s.log", target.GlobalSeq, reason))
	if writeErr := os.WriteFile(logPath, out, 0o644); writeErr != nil {
		return "", fmt.Errorf("write docker log %s: %w", logPath, writeErr)
	}

	return logPath, nil
}

func bootstrapNodesAndGenesis(ctx context.Context, spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	if spec.Binary == "" {
		return fmt.Errorf("benchmark.binary is required for bootstrap")
	}

	// Scratch space: every validator's gentx for collect-gentxs, and the bulk
	// account list.
	workDir, err := os.MkdirTemp("", "evm-benchmark-genesis-")
	if err != nil {
		return fmt.Errorf("create genesis work dir: %w", err)
	}
	defer os.RemoveAll(workDir)
	gentxDir := filepath.Join(workDir, "gentx")
	if err := os.Mkdir(gentxDir, 0o755); err != nil {
		return fmt.Errorf("create gentx dir: %w", err)
	}
	bulkPath := filepath.Join(workDir, "accounts.json")

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

		validatorKey, err := keygen.DeterministicKey(node.GlobalSeq, 0, spec.BaseMnemonic)
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

			if _, err := chainCmd(ctx, spec.Binary, nil,
				"genesis", "gentx", validatorKeyName, validatorStakedCoins+spec.Denom,
				"--home", node.Home,
				"--chain-id", spec.ChainID,
				"--keyring-backend", "test",
				"--output-document", filepath.Join(gentxDir, fmt.Sprintf("gentx-%d.json", node.GlobalSeq)),
				"--min-self-delegation", "1",
			); err != nil {
				return fmt.Errorf("gentx for validator node %d: %w", node.GlobalSeq, err)
			}
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
			addr, err := bech32Address(node.GlobalSeq, 0, spec.AddressPrefix, spec.BaseMnemonic)
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
			acct, err := bech32Address(node.GlobalSeq, i, spec.AddressPrefix, spec.BaseMnemonic)
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

	if err := writeJSON(bulkPath, accounts); err != nil {
		return err
	}
	leaderGenesis := filepath.Join(leaderHome, "config", "genesis.json")

	if _, err := chainCmd(ctx, spec.Binary, nil,
		"genesis", "bulk-add-genesis-account", bulkPath,
		"--home", leaderHome,
	); err != nil {
		return fmt.Errorf("bulk add genesis accounts: %w", err)
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
	peerEntries := make([]string, 0, len(nodes)-1)
	for _, n := range nodes {
		if n.Home == home {
			continue
		}
		nodeIDBz, err := chainCmd(context.Background(), spec.Binary, nil, "comet", "show-node-id", "--home", n.Home)
		if err != nil {
			return fmt.Errorf("read node id for %s: %w", n.Home, err)
		}
		peerEntries = append(peerEntries, fmt.Sprintf("%s@%s:26656", strings.TrimSpace(string(nodeIDBz)), n.Hostname))
	}

	if err := editToml(home, "config.toml", func(cfg map[string]any) {
		setNested(cfg, []string{"p2p", "persistent_peers"}, strings.Join(peerEntries, ","))
		setNested(cfg, []string{"mempool", "recheck"}, false)
		setNested(cfg, []string{"mempool", "size"}, int64(10000))
		setNested(cfg, []string{"consensus", "timeout_commit"}, "1s")
		if spec.RunnerType == "docker" {
			setNested(cfg, []string{"rpc", "laddr"}, "tcp://0.0.0.0:26657")
		}
		config.DeepMerge(cfg, spec.ConfigPatch)
	}); err != nil {
		return err
	}

	if err := editToml(home, "app.toml", func(cfg map[string]any) {
		setNested(cfg, []string{"minimum-gas-prices"}, fmt.Sprintf("0%s", spec.Denom))
		// Size the EVM mempool to the load, the same way the reth families derive
		// their --txpool.* flags. evmd's defaults hold ~6k transactions
		// (global-slots 5120, global-queue 1024, account-slots 16), so anything
		// past them is refused at submission and the run measures rejection. These
		// are set before app_patch merges, so an operator can still override them.
		for key, value := range evmMempoolSizes(spec) {
			setNested(cfg, []string{"evm", "mempool", key}, value)
		}
		setNested(cfg, []string{"json-rpc", "enable"}, true)
		if spec.RunnerType == "docker" {
			setNested(cfg, []string{"json-rpc", "address"}, "0.0.0.0:8545")
			setNested(cfg, []string{"json-rpc", "ws-address"}, "0.0.0.0:8546")
		}
		config.DeepMerge(cfg, spec.AppPatch)
	}); err != nil {
		return err
	}

	return editToml(home, "client.toml", func(cfg map[string]any) {
		if strings.TrimSpace(spec.ChainID) != "" {
			setNested(cfg, []string{"chain-id"}, spec.ChainID)
		}
	})
}

// editToml rewrites a file in the node's config directory through edit.
func editToml(home, name string, edit func(map[string]any)) error {
	path := filepath.Join(home, "config", name)
	in, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	var cfg map[string]any
	if err := tomlv2.Unmarshal(in, &cfg); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	edit(cfg)
	out, err := tomlv2.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encode %s: %w", name, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// evmMempoolSizes renders poolCapacity as evmd's app.toml mempool keys.
func evmMempoolSizes(spec messages.BenchmarkSpec) map[string]any {
	count, slots := poolCapacity(spec)
	return map[string]any{
		"global-slots":      int64(count),
		"global-queue":      int64(count),
		"account-slots":     int64(slots),
		"account-queue":     int64(slots),
		"insert-queue-size": strconv.Itoa(max(count/10, 5000)),
	}
}

func setNested(m map[string]any, keys []string, val any) {
	last := len(keys) - 1
	for _, k := range keys[:last] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[keys[last]] = val
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

func bech32Address(globalSeq, index int, prefix, baseMnemonic string) (string, error) {
	key, err := keygen.DeterministicKey(globalSeq, index, baseMnemonic)
	if err != nil {
		return "", err
	}
	return ethAddressToBech32(crypto.PubkeyToAddress(key.PublicKey), prefix)
}

// patchGenesisContractAuthAccount gives the test token an auth account, unless
// genesis has one already.
func patchGenesisContractAuthAccount(genesis []byte, prefix string) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(genesis, &doc); err != nil {
		return nil, fmt.Errorf("decode genesis json: %w", err)
	}

	appState, ok := doc["app_state"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("genesis missing app_state")
	}
	authState, ok := appState["auth"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("genesis missing app_state.auth")
	}

	contractAddr, err := ethAddressToBech32(common.HexToAddress(erc20ContractAddress), prefix)
	if err != nil {
		return nil, err
	}
	accounts, _ := authState["accounts"].([]any)
	for _, entry := range accounts {
		if m, ok := entry.(map[string]any); ok && m["address"] == contractAddr {
			return genesis, nil
		}
	}

	authState["accounts"] = append(accounts, map[string]any{
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

// runtimeGeneratedTxs reports whether this run's transactions were produced by
// the chain runtime itself (e.g. Tempo's native 0x76 signer) rather than the
// built-in legacy signer, in which case they must not be re-signed by
// internal/bench.
func runtimeGeneratedTxs(spec messages.BenchmarkSpec) bool {
	chain, err := resolveRuntime(spec)
	if err != nil {
		return false
	}
	// Ask the producer itself whether it is configured, so a future family
	// implementing txProducer is not silently re-signed by the legacy signer.
	producer, ok := chain.(txProducer)
	return ok && producer.ProducesTxs(spec)
}
