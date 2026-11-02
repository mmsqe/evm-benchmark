package main

import (
	"flag"
	"log"

	"github.com/mmsqe/evm-benchmark/internal/activities"
	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/workflows"
	"go.temporal.io/sdk/worker"
)

var configPath = flag.String("config", "./examples/config.jsonnet", "Path to worker config")

func main() {
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	c, err := cfg.Temporal.Dial()
	if err != nil {
		log.Fatalf("dial temporal: %v", err)
	}
	defer c.Close()

	w := worker.New(c, cfg.Temporal.TaskQueue, worker.Options{})
	w.RegisterWorkflow(workflows.StatelessEVMBenchmarkWorkflow)
	w.RegisterActivity(&activities.Activity{}) // every method, under its own name

	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("run worker: %v", err)
	}
}
