package workflows

import (
	"time"

	"github.com/mmsqe/evm-benchmark/internal/messages"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

func StatelessEVMBenchmarkWorkflow(ctx workflow.Context, req messages.WorkflowRequest) (messages.WorkflowResponse, error) {
	opts := workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Hour,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, opts)

	var layoutNodes []messages.NodeTarget
	if req.Spec.SkipGenerateLayout {
		var loaded messages.LoadLayoutResponse
		if err := workflow.ExecuteActivity(ctx, "LoadLayout", messages.LoadLayoutRequest{Spec: req.Spec}).Get(ctx, &loaded); err != nil {
			return messages.WorkflowResponse{}, err
		}
		layoutNodes = loaded.Nodes
	} else {
		var layout messages.GenerateLayoutResponse
		if err := workflow.ExecuteActivity(ctx, "GenerateLayout", messages.GenerateLayoutRequest{Spec: req.Spec}).Get(ctx, &layout); err != nil {
			return messages.WorkflowResponse{}, err
		}
		layoutNodes = layout.Nodes
	}

	dockerImageOverride := ""
	if req.Spec.PatchImage.Enabled {
		var patchRes messages.PatchImageResponse
		if err := workflow.ExecuteActivity(ctx, "PatchImage", messages.PatchImageRequest{Spec: req.Spec}).Get(ctx, &patchRes); err != nil {
			return messages.WorkflowResponse{}, err
		}
		dockerImageOverride = patchRes.ImageTag
	}

	if req.Spec.PreGenerateTxs {
		futures := make([]workflow.Future, 0, len(layoutNodes))
		for _, n := range layoutNodes {
			f := workflow.ExecuteActivity(ctx, "GenerateTxs", messages.GenerateTxsRequest{Spec: req.Spec, Target: n})
			futures = append(futures, f)
		}
		for _, f := range futures {
			var txCount int
			if err := f.Get(ctx, &txCount); err != nil {
				return messages.WorkflowResponse{}, err
			}
		}
	}

	result := messages.WorkflowResponse{}
	if req.Spec.RunNodes {
		nodeFutures := make([]workflow.Future, 0, len(layoutNodes))
		for _, n := range layoutNodes {
			f := workflow.ExecuteActivity(ctx, "RunNode", messages.RunNodeRequest{Spec: req.Spec, Target: n, DockerImageOverride: dockerImageOverride})
			nodeFutures = append(nodeFutures, f)
		}

		for _, f := range nodeFutures {
			var nodeRes messages.NodeRunResult
			if err := f.Get(ctx, &nodeRes); err != nil {
				return messages.WorkflowResponse{}, err
			}
			result.NodeResults = append(result.NodeResults, nodeRes)
		}
	}

	return result, nil
}
