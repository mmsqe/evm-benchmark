// reth validation A/B: the producer plus one fullnode following it, so a
// validation change can be compared on identical, full blocks. See "Reth Mode"
// in the README.
//
//   scripts/run-benchmark.sh --mode reth --config examples/config.reth.validation.jsonnet run
local reth = import 'config.reth.jsonnet';

reth {
  benchmark+: {
    // Caps a block at ~35k of these transfers, so every block is full.
    reth_gas_limit: 1000000000,
    // Without reth's intake tuning and Osaka lift: these blocks stay under
    // Osaka's size cap.
    reth_node_args: [],
    genesis_patch:: {},

    // Fullnode-only flags, e.g. ['--engine.prewarm-reuse'].
    reth_fullnode_args: [],

    // This share of each sender's transfers writes one balance slot.
    hot_recipient_percent: 30,

    // 2 nodes x 2,000 x 100 = 400,000.
    fullnodes: 1,
    num_accounts: 2000,
    num_txs: 100,
    broadcast_concurrency: 32,
    broadcast_batch_size: 100,
  },
}
