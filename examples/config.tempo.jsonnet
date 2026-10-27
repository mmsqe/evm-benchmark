// Tempo (commonware consensus) benchmark, local runner.
//
// Prerequisites: the `tempo` and `tempo-xtask` binaries only. The devnet is
// generated in-process via `tempo-xtask generate-localnet`, and native 0x76
// transactions are signed in-process too.
//
// Preflight before trusting any number:
//   go run ./cmd/checkfunding -rpc http://127.0.0.1:8004 -chain-id 1337
local base = import 'base.libsonnet';

base {
  start: { workflow_id: 'tempo-benchmark-sample' },

  benchmark+: {
    data_dir: '/tmp/tempo-benchmark/data',
    out_dir: '/tmp/tempo-benchmark/output',

    chain_family: 'tempo',

    // Resolved from PATH. Override with an absolute path (~ / $VAR allowed) to
    // benchmark a specific local build instead.
    tempo_bin: 'tempo',
    tempo_xtask_bin: 'tempo-xtask',

    // Ports are allocated in blocks of 6 per node; JSON-RPC sits at base+4,
    // i.e. node0 serves http://127.0.0.1:8004.
    tempo_base_port: 8000,
    tempo_epoch_length: 100,
    // Per-block gas budget. With Tempo's ~271k intrinsic floor this caps a block
    // at gas_limit/271k transactions, so it bounds TPS directly — set it
    // deliberately and report it alongside any result.
    tempo_gas_limit: 3000000000,

    // Extra flags appended to every `tempo node` launcher, last, so they override
    // the defaults. If you raise target-block-time, raise wait-for-proposal above
    // it and wait-for-notarizations above that, or the validator times out its
    // own view and consensus stalls at height 0, or tempo refuses to start.
    // tempo_node_args: [
    //   '--consensus.target-block-time', '2s',
    //   '--consensus.wait-for-proposal', '4s',
    //   '--consensus.wait-for-notarizations', '5s',
    // ],

    evm_chain_id: 1337,
    // Must match the devnet mnemonic: the benchmark signs from
    // m/44'/60'/0'/0/{1..num_accounts}, which tempo-xtask funds from index 0.
    base_mnemonic: 'test test test test test test test test test test test junk',

    // Set `tempo_legacy_txs: true` to fall back to legacy/London EVM transactions
    // (Tempo's compatibility path); that path is single-validator only.
    tempo_max_priority_fee_per_gas: 1000000000,
    // Key 0 is the account's own nonce; user keys cost 22,100 gas on a key's
    // first transaction and 5,000 after. Report it with any number.
    tempo_nonce_key: 0,
    // Spread each account's txs across this many nonce lanes (nonce_key = base +
    // lane). 1 = sequential. With num_accounts: 1 and tempo_tx_shape: approve, a
    // high lane count makes one owner issue parallel-eligible approves that all
    // write the same allowance slot — the conflict workload.
    // tempo_nonce_lanes: 8,
    // Workload shape; see "Transaction shape" in the README for the list and each
    // one's gas floor. State which one a number came from.
    tempo_tx_shape: 'self',
    // tempo_batch_calls: 4,  // only used by tempo_tx_shape: batch

    // Tempo rejects native value transfers ("value transfer not allowed"), so the
    // workload is a TIP-20 transfer. The default target is the genesis fee token
    // 0x20c0…0000; no contract deployment is required.
    tx_type: 'erc20-transfer',
    erc20_contract_address: '0x20c0000000000000000000000000000000000000',
    // Must cover the shape's gas floor (README table) or every tx is rejected up
    // front.
    erc20_transfer_gas: 300000,
    // Native txs are signed once with this as max_fee_per_gas, not regenerated,
    // and the base fee rose to ~8 gwei under load in earlier runs, so keep
    // meaningful headroom.
    gas_price_wei: 40000000000,

    num_accounts: 2000,
    // 2,000,000 total, about 20 whole seconds at this chain's rate: 500,000
    // drained in 6, too few seconds to average out block-to-block jitter. evmd
    // uses a smaller one; see "Comparing chains" in the README.
    num_txs: 1000,
  },
}
