// evmd benchmark, docker runner: config.local.jsonnet's chain and load, run in
// the image `scripts/run-benchmark.sh prepare` patches with the generated data.
local evmd = import 'config.local.jsonnet';

evmd {
  start: { workflow_id: 'evm-benchmark-sample' },

  benchmark+: {
    data_dir: '/private/tmp/data/out',
    out_dir: '/tmp/evm-benchmark/output',

    runner_type: 'docker',
    docker_image: 'evmd-benchmark-patched:local',
    docker_keep_containers: true,
    docker_volumes: ['/tmp/colima:/outputs'],
    docker_env: { JOB_COMPLETION_INDEX: '0' },

    // patchimage stage
    patch_image: {
      to_image: 'evmd-benchmark-patched:local',
      source_dir: '/private/tmp/data/out',
    },
  },
}
