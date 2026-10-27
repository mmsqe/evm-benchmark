// Tempo benchmark against containerised validators: config.tempo.jsonnet's
// devnet and workload on the same ports, with the cluster started by
// `docker compose up -d` rather than a per-node `docker run`. tempo_bin is now
// the in-image command name; tempo_xtask_bin still runs on the host to build
// genesis and keys. Raised consensus timeouts in tempo_node_args matter doubly
// here, with real multi-validator BFT.
//
// This measures the tempo build inside the image, not a local binary. Check it
// with:
//   docker run --rm --entrypoint sh <image> -c "tempo --version"
local tempo = import 'config.tempo.jsonnet';

tempo {
  start: { workflow_id: 'tempo-docker-benchmark' },

  benchmark+: {
    // data_dir must be under a path the Docker VM shares into containers (each
    // node dir is bind-mounted to /data); colima shares only $HOME and /tmp/colima.
    data_dir: '~/tempo-benchmark-docker/data',
    out_dir: '~/tempo-benchmark-docker/output',

    runner_type: 'docker',
    // Compose owns the nodes; RunNode only waits for the published RPC.
    start_node: false,

    tempo_docker_image: 'ghcr.io/tempoxyz/tempo:latest',
    tempo_docker_network: 'tempo-devnet',
    // Fixed so `scripts/run-benchmark.sh --mode tempo-docker stop` can find it.
    tempo_compose_project: 'evm-benchmark-tempo',

    // >=2 required: the docker launcher builds each container's trusted-peers from the
    // OTHER validators, so a single-node docker devnet gets an empty list.
    // 4 also makes this the first multi-validator measurement (real consensus).
    validators: 4,
    // Per node. Each node takes a disjoint slice of the funded account branch,
    // so genesis funds validators * num_accounts + 1 accounts (index 0 is the
    // validator key). 4 nodes x 500 x 1,000 = 2,000,000 in all, the load
    // config.tempo.jsonnet sends.
    num_accounts: 500,
  },
}
