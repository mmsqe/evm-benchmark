package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mmsqe/evm-benchmark/internal/activities"
	"github.com/mmsqe/evm-benchmark/internal/config"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "build-image":
		runBuildImage(os.Args[2:])
	case "gen":
		runGen(os.Args[2:])
	case "patchimage":
		runPatchImage(os.Args[2:])
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`benchctl: local benchmark helper commands

Usage:
  go run ./cmd/benchctl build-image [flags]
  go run ./cmd/benchctl gen [flags]
  go run ./cmd/benchctl patchimage [flags]

Commands:
  build-image   Build a base docker image with docker build
  gen           Generate benchmark layout and genesis data (generic-gen equivalent)
	patchimage    Build patched image by ADD-ing generated out dir into /data
`)
}

func runBuildImage(args []string) {
	fs := flag.NewFlagSet("build-image", flag.ExitOnError)
	dockerfile := fs.String("dockerfile", "./docker/base.Dockerfile", "Dockerfile path")
	contextDir := fs.String("context", ".", "docker build context")
	tag := fs.String("tag", "", "target image tag (required)")
	buildArgs := fs.String("build-args", "", "comma-separated build args KEY=VALUE,KEY2=VALUE2")
	fs.Parse(args)

	if strings.TrimSpace(*tag) == "" {
		log.Fatal("-tag is required")
	}

	cmdArgs := []string{"build", "-f", *dockerfile, "-t", *tag}
	if strings.TrimSpace(*buildArgs) != "" {
		for _, entry := range strings.Split(*buildArgs, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			cmdArgs = append(cmdArgs, "--build-arg", entry)
		}
	}
	cmdArgs = append(cmdArgs, *contextDir)

	if err := runCommand("docker", cmdArgs...); err != nil {
		log.Fatalf("build image: %v", err)
	}
	fmt.Printf("built image: %s\n", *tag)
}

func runGen(args []string) {
	fs := flag.NewFlagSet("gen", flag.ExitOnError)
	configPath := fs.String("config", "./examples/config.yaml", "config path")
	dataRoot := fs.String("data-root", "", "root data dir; writes generated data to <data-root>/out")
	clean := fs.Bool("clean", false, "remove data-root before generation")
	fs.Parse(args)

	cfg, err := config.LoadForGenerate(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	spec := cfg.Benchmark
	if strings.TrimSpace(*dataRoot) != "" {
		if *clean {
			if err := os.RemoveAll(*dataRoot); err != nil {
				log.Fatalf("clean data root: %v", err)
			}
		}
		spec.DataDir = filepath.Join(*dataRoot, "out")
	}

	act := &activities.Activity{}
	res, err := act.GenerateLayout(context.Background(), messages.GenerateLayoutRequest{Spec: spec})
	if err != nil {
		log.Fatalf("generate layout: %v", err)
	}

	fmt.Printf("generated nodes: %d\n", len(res.Nodes))
	fmt.Printf("data dir: %s\n", spec.DataDir)
}

func runPatchImage(args []string) {
	fs := flag.NewFlagSet("patchimage", flag.ExitOnError)
	configPath := fs.String("config", "./examples/config.yaml", "config path")
	fromImage := fs.String("from-image", "", "base image tag (overrides config)")
	toImage := fs.String("to-image", "", "patched image tag (overrides config)")
	sourceDir := fs.String("source-dir", "", "source directory to ADD as ./out (overrides config)")
	dst := fs.String("dst", "", "destination path in image (overrides config)")
	fs.Parse(args)

	cfg, err := config.LoadForGenerate(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	spec := cfg.Benchmark
	if strings.TrimSpace(*fromImage) != "" {
		spec.PatchImageFromImage = *fromImage
	}
	if strings.TrimSpace(*toImage) != "" {
		spec.PatchImageToImage = *toImage
	}
	if strings.TrimSpace(*sourceDir) != "" {
		spec.PatchImageSourceDir = *sourceDir
	}
	if strings.TrimSpace(*dst) != "" {
		spec.PatchImageDest = *dst
	}

	act := &activities.Activity{}
	res, err := act.PatchImage(context.Background(), messages.PatchImageRequest{Spec: spec})
	if err != nil {
		log.Fatalf("patch image: %v", err)
	}

	fmt.Printf("patched image: %s\n", res.ImageTag)
	fmt.Printf("from image:    %s\n", res.FromImage)
	fmt.Printf("source dir:    %s\n", res.SourceDir)
	fmt.Printf("dest path:     %s\n", res.TargetDest)
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
