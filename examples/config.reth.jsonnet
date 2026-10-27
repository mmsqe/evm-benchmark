// reth benchmark, local runner: one `reth node --dev` producer taking the whole
// load. See "Reth Mode" in the README; config.reth.validation.jsonnet compares a
// validation change instead.
//
//   scripts/run-benchmark.sh --mode reth run
//
// Preflight before trusting any number (node0 serves base_port + 2):
//   go run ./cmd/checkfunding -rpc http://127.0.0.1:9502 -chain-id 1337 -fee-token ""
local base = import 'base.libsonnet';

base {
  start: { workflow_id: 'reth-benchmark-sample' },

  benchmark+: {
    data_dir: '/tmp/reth-benchmark/data',
    out_dir: '/tmp/reth-benchmark/output',

    chain_family: 'reth',

    // Resolved from PATH, or an absolute path (~ / $VAR allowed) to a local build.
    reth_bin: 'reth',
    // 5 ports per node: node0 serves JSON-RPC on 9502, node1 on 9507.
    reth_base_port: 9500,
    // No block fills this; Osaka's size cap is lifted below.
    reth_gas_limit: 10000000000,
    reth_block_time: '1s',

    // Transaction intake, not block building, is where the node saturates.
    reth_node_args: ['--txpool.additional-validation-tasks', '8'],

    evm_chain_id: 1337,
    base_mnemonic: 'test test test test test test test test test test test junk',

    // Uncontended, like tempo's default shape.
    tx_type: 'erc20-transfer',
    erc20_transfer_gas: 60000,
    hot_recipient_percent: 0,
    gas_price_wei: 10000000000,

    // 40,000 x 50 = 2,000,000, all to the producer: a fullnode's share would
    // arrive by gossip, slower than the blocks. Few transactions per account, so
    // few arrive ahead of their nonce.
    num_accounts: 40000,
    num_txs: 50,
    num_idle: 10,
    broadcast_concurrency: 64,
    broadcast_batch_size: 200,

    // Not a mainnet fork schedule: Osaka's ~8 MiB block size limit would cap a
    // block at ~48k of these transfers.
    genesis_patch: {
      config: { osakaTime: null },
    },
  },
}
