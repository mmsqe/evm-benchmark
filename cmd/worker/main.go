package main

import (
	"flag"
	"log"

	"github.com/mmsqe/evm-benchmark/internal/activities"
	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/workflows"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

var configPath = flag.String("config", "./examples/config.yaml", "Path to worker config")

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

	w := worker.New(c, cfg.Temporal.TaskQueue, worker.Options{})

	act := &activities.Activity{}
	w.RegisterWorkflow(workflows.StatelessEVMBenchmarkWorkflow)
	w.RegisterActivityWithOptions(act.GenerateLayout, activity.RegisterOptions{Name: "GenerateLayout"})
	w.RegisterActivityWithOptions(act.PatchImage, activity.RegisterOptions{Name: "PatchImage"})
	w.RegisterActivityWithOptions(act.GenerateTxs, activity.RegisterOptions{Name: "GenerateTxs"})
	w.RegisterActivityWithOptions(act.RunNode, activity.RegisterOptions{Name: "RunNode"})

	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("run worker: %v", err)
	}
}
