// evmd benchmark, local runner. The chain comes from the `evmd` profile in
// config/chains.jsonnet; config.jsonnet runs the same load in docker.
local base = import 'base.libsonnet';

base {
  start: { workflow_id: 'evm-benchmark-local' },

  benchmark+: {
    data_dir: '/private/tmp/evm-benchmark-local/data/out',
    out_dir: '/private/tmp/evm-benchmark-local/output',

    // chain profile
    chain_config: 'evmd',
    chains_config_path: '../config/chains.jsonnet',
    // optional: if set, account keys are derived from this phrase deterministically
    base_mnemonic: 'notable error gospel wave pair ugly measure elite toddler cost various fly make eye ketchup despair slab throw tribe swarm word fruit into inmate',

    // Deep-merged into config.toml, app.toml and genesis.json.
    config_patch: {
      log_level: 'info',
      mempool: {
        // type "app" hands the mempool to the EVM app, so this size is inert —
        // the capacity that matters is app_patch.evm.mempool below.
        type: 'app',
        size: 50000,
      },
      consensus: { timeout_commit: '20ms' },
    },
    app_patch: {
      evm: { mempool: { 'operate-exclusively': true } },
      'json-rpc': { api: 'eth,net,web3,txpool' },
    },
    genesis_patch: {
      consensus: { params: { block: {
        // Far above what a block actually fills, so gas is not the binding
        // constraint — matching the tempo and allegro examples.
        max_gas: '3000000000',
      } } },
    },

    // 200,000 total. Sized per chain, not shared: evmd cannot finish the 500,000
    // the tempo and allegro examples use, and full_second is a rate, so runs of
    // different sizes compare as long as each is saturated and spans >= 5 whole
    // seconds. 8,000 senders x 40 measured 2,127 tx/s against 6,695 for the same
    // 200k load in this shape: more senders make evmd's proposal assembly walk
    // more accounts per block, and it built 103-tx blocks instead of ~700. Load
    // goes up via accounts only to ~100 txs each. See "Comparing chains" in the
    // README.
    num_accounts: 2000,
    num_txs: 100,

    // Reuse data_dir/nodes.json and the node homes instead of regenerating them.
    skip_generate_layout: true,
  },
}
