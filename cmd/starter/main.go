package main

import (
	"context"
	"flag"
	"fmt"
	"log"

	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/workflows"
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
	})
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	run, err := c.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
		ID:        cfg.Start.WorkflowID,
		TaskQueue: cfg.Temporal.TaskQueue,
	}, workflows.StatelessEVMBenchmarkWorkflow, messages.WorkflowRequest{Spec: cfg.Benchmark})
	if err != nil {
		log.Fatalf("start workflow: %v", err)
	}

	fmt.Printf("workflow started: id=%s run_id=%s\n", run.GetID(), run.GetRunID())

	var res messages.WorkflowResponse
	if err := run.Get(context.Background(), &res); err != nil {
		log.Fatalf("workflow failed: %v", err)
	}

	fmt.Println("workflow completed")
	for _, r := range res.NodeResults {
		fmt.Printf("node=%d txs=%d top_tps=%v stats=%s\n", r.GlobalSeq, r.TxsSent, r.TopTPS, r.StatsFile)
	}
}
