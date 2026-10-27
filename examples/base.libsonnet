// What every example shares; see "Configs" in the README.
{
  temporal: {
    host_port: '127.0.0.1:7233',
    namespace: 'default',
    task_queue: 'EVM_BENCHMARK_TASK_QUEUE',
  },

  benchmark: {
    runner_type: 'local',
    start_node: true,

    // benchmark shape: one node takes the whole load, the same in every example
    // compared in "Comparing chains" in the README.
    validators: 1,
    fullnodes: 0,
    // Blocks, not seconds: the run stops once this many consecutive blocks are
    // empty. A fast chain produces tens of blocks per second, so a smaller
    // value ends the run before the pool has drained and reports included=0
    // for a chain that was fine.
    num_idle: 40,
    // Caps concurrent eth_sendRawTransaction requests, so local sockets are not
    // exhausted.
    broadcast_concurrency: 32,
    // Transactions per JSON-RPC request; one round trip each caps the sender
    // near 50k tx/s. See "Measuring" in the README.
    broadcast_batch_size: 100,
    // Negative: never pause. The default (5,000 pending) caps the backlog, so a
    // chain that drains faster than the sender refills is measured on the
    // sender's loop instead of its own execution.
    broadcast_pending_watermark: -1,

    // workflow behavior
    skip_generate_layout: false,
    pre_generate_txs: true,
    run_nodes: true,
    validator_generate_load: true,
  },
}
