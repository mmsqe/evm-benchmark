// Tempo public testnet (Moderato) through its public RPC. Nothing is started:
// senders are funded from the faucet and only this run's transactions count.
//
// The rate reflects a shared, rate-limited gateway as much as the chain, so it
// is not comparable with the devnet examples. Keep the load small.
//
// Sign with your own mnemonic (the devnet one is refused):
//   export TEMPO_BENCH_MNEMONIC="$(cast wallet new-mnemonic | sed -n '/^Phrase:/{n;p;}')"
local base = import 'base.libsonnet';

base {
  start: { workflow_id: 'tempo-moderato-benchmark' },

  benchmark+: {
    data_dir: '/tmp/tempo-remote-benchmark/data',
    out_dir: '/tmp/tempo-remote-benchmark/output',

    chain_family: 'tempo',
    remote_rpc_url: 'https://rpc.moderato.tempo.xyz',
    evm_chain_id: 42431,
    base_mnemonic: '${TEMPO_BENCH_MNEMONIC}',
    tempo_faucet: true,
    start_node: false,

    tempo_tx_shape: 'self',
    tx_type: 'erc20-transfer',
    erc20_contract_address: '0x20c0000000000000000000000000000000000000',
    erc20_transfer_gas: 300000,
    // Max fee; the base fee is 0.6 gwei.
    gas_price_wei: 3000000000,
    tempo_max_priority_fee_per_gas: 1000000000,

    // 2D nonces: fewer accounts carry the load, off each account's own nonce.
    // Costs 22,100 gas on a key's first transaction, 5,000 after.
    // tempo_nonce_key: 1000,
    // tempo_nonce_lanes: 4,

    num_accounts: 20,
    num_txs: 25,
    // The gateway refuses 10 eth_sendRawTransaction per batch and accepts 5; the
    // balance and nonce reads use the same cap.
    broadcast_concurrency: 2,
    broadcast_batch_size: 5,
    // A node that answers requests one at a time makes each wait for those ahead
    // of it: allow (workers x batch) / (chain's admission rate), with margin.
    broadcast_request_timeout_seconds: 60,
  },
}
