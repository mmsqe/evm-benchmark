package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

var configPath = flag.String("config", "./examples/config.yaml", "Path to starter config")

func main() {
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	c, err := client.Dial(client.Options{
		HostPort:  cfg.Temporal.HostPort,
		Namespace: cfg.Temporal.Namespace,
		ConnectionOptions: client.ConnectionOptions{
			DisableKeepAliveCheck:               true,
			DisableKeepAlivePermitWithoutStream: true,
			GetSystemInfoTimeout:                15 * time.Second,
		},
	})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	startOpts := client.StartWorkflowOptions{
		ID:        cfg.Start.WorkflowID,
		TaskQueue: cfg.Temporal.TaskQueue,
		// Return an explicit error when the workflow ID is already running.
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}

	run, err := c.ExecuteWorkflow(context.Background(), startOpts, workflows.StatelessEVMBenchmarkWorkflow, messages.WorkflowRequest{Spec: cfg.Benchmark})
	startedNew := true
	if err != nil {
		var alreadyStartedErr *serviceerror.WorkflowExecutionAlreadyStarted
		if errors.As(err, &alreadyStartedErr) {
			startedNew = false
			run = c.GetWorkflow(context.Background(), cfg.Start.WorkflowID, "")
		} else {
			log.Fatalf("start workflow: %v", err)
		}
	}

	if startedNew {
		fmt.Printf("workflow started: id=%s run_id=%s\n", run.GetID(), run.GetRunID())
	} else {
		fmt.Printf("workflow already running: id=%s (attached to existing execution)\n", run.GetID())
	}

	var res messages.WorkflowResponse
	retryDelay := 2 * time.Second
	for {
		err := run.Get(context.Background(), &res)
		if err == nil {
			break
		}
		if !isRetryableGetError(err) {
			log.Fatalf("workflow failed: %v", err)
		}
		log.Printf("workflow wait transient error: %v; retrying in %s", err, retryDelay)
		time.Sleep(retryDelay)
		if retryDelay < 30*time.Second {
			retryDelay *= 2
			if retryDelay > 30*time.Second {
				retryDelay = 30 * time.Second
			}
		}
	}

	fmt.Println("workflow completed")
	for _, r := range res.NodeResults {
		if err := printHeightTxLines(r.GlobalSeq, r.StatsFile); err != nil {
			log.Printf("failed to print per-height tx stats for node=%d: %v", r.GlobalSeq, err)
		}
		fmt.Printf("node=%d sent=%d included=%d pending=%d top_tps=%v\n", r.GlobalSeq, r.TxsSent, r.IncludedTxs, r.PendingTxpool, r.TopTPS)
	}
}

func printHeightTxLines(node int, statsPath string) error {
	f, err := os.Open(statsPath)
	if err != nil {
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if strings.HasPrefix(line, "height=") {
			fmt.Printf("node=%d %s\n", node, line)
		}
	}

	return s.Err()
}

func isRetryableGetError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var deadlineErr *serviceerror.DeadlineExceeded
	if errors.As(err, &deadlineErr) {
		return true
	}

	var unavailableErr *serviceerror.Unavailable
	if errors.As(err, &unavailableErr) {
		return true
	}

	return false
}
