// Allegro (reth + commonware simplex consensus) benchmark, local runner.
//
// Prerequisites: the `allegro` and `allegro-xtask` binaries only. The devnet is
// generated in-process via `allegro-xtask genesis`, and the load is ordinary
// legacy EVM transactions from the shared signer — allegro is a stock reth
// execution node, so nothing about the tx path is family-specific.
//
// Preflight before trusting any number (node0 serves base_port + 3):
//   go run ./cmd/checkfunding -rpc http://127.0.0.1:9003 -chain-id 1337 -fee-token ""
local base = import 'base.libsonnet';

base {
  start: { workflow_id: 'allegro-benchmark-sample' },

  benchmark+: {
    data_dir: '/tmp/allegro-benchmark/data',
    out_dir: '/tmp/allegro-benchmark/output',

    chain_family: 'allegro',

    // Resolved from PATH. Override with an absolute path (~ / $VAR allowed) to
    // benchmark a specific local build instead, e.g.
    //   allegro_bin: '~/Documents/crypto/allegro/target/release/allegro',
    allegro_bin: 'allegro',
    allegro_xtask_bin: 'allegro-xtask',

    // Ports are allocated in blocks of 4 per node (consensus p2p, execution p2p,
    // authrpc, http); JSON-RPC sits at base+3, i.e. node0 serves
    // http://127.0.0.1:9003 and node1 http://127.0.0.1:9007.
    allegro_base_port: 9000,

    // Per-block gas budget, written to genesis and pinned as the builder target.
    // At 21,000 gas per transfer this caps a block at gas_limit/21000 txs, so it
    // bounds TPS directly — report it alongside any result. Leave unset to keep
    // allegro-xtask's 30M default (~1,428 transfers per block). 3e9 is far above
    // what a block actually fills, so gas is not the binding constraint — the
    // same is true of the tempo and evmd examples, which makes the three
    // comparable (see "Comparing chains" in the README).
    allegro_gas_limit: 3000000000,

    // Consensus block cadence. Unset means the binary's own defaults (2000/4000).
    allegro_leader_timeout_ms: 1000,
    allegro_cert_timeout_ms: 2000,

    // Extra flags appended to every generated `allegro node` launcher. The reth
    // payload job rebuilds on this interval, so lowering it lets a block pick up
    // transactions that arrived after the build started.
    // allegro_node_args: ['--builder.interval', '200ms'],

    evm_chain_id: 1337,
    // allegro-xtask prefunds the anvil accounts; the benchmark additionally funds
    // every account it signs from (m/44'/60'/{node}'/0/{1..num_accounts}) into the
    // generated genesis, so this only has to match what you want to sign with.
    base_mnemonic: 'test test test test test test test test test test test junk',

    // Allegro is a stock reth EVM: native value transfers are the cheapest shape
    // and no contract has to be deployed. erc20-transfer is rejected unless you
    // also supply the contract's code via genesis_patch.alloc.
    tx_type: 'simple-transfer',
    simple_transfer_gas: 21000,
    // Headroom over the EIP-1559 base fee; the run also re-reads eth_gasPrice and
    // regenerates the transactions if the chain asks for more.
    gas_price_wei: 2000000000,

    num_accounts: 2000,
    // 500,000 total: at this chain's rate a smaller load drains in a few seconds
    // and the run is bounded by the load rather than the chain. evmd uses a
    // smaller one; see "Comparing chains" in the README.
    num_txs: 250,
  },
}
