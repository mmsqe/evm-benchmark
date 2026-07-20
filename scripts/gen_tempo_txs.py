#!/usr/bin/env python
"""Generate native Tempo (`0x76`) transactions for the benchmark.

The load generator's built-in signer emits legacy/London EVM transactions,
which Tempo accepts through its compatibility path. Real Tempo workloads use
the native envelope instead, which carries batched `calls`, a per-transaction
`fee_token` and `nonce_key` lanes. This script produces that shape using
tempo-py's canonical encoder, so the encoding is never reimplemented here.

Output is the same artifact the Go path writes: a JSON array of raw hex
strings at ``--out``, which ``RunNode`` broadcasts unchanged.

Keys use the same BIP32 scheme as ``internal/keygen/deterministic.go``
(``m/44'/60'/{global_seq}'/0/{index}``), but the branch and index are chosen
differently for multi-node runs: ``tempo-xtask`` funds only branch 0, so every
node stays on ``--global-seq 0`` and takes a disjoint slice of it via
``--account-offset`` instead of using its own (unfunded) branch the way the Go
generator does.

Run it with an interpreter that can import ``tempo`` (tempo-py's venv).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
from concurrent.futures import ProcessPoolExecutor

from eth_account import Account
from tempo import Builder, Signer, serialize, sign_transaction
from tempo.contracts import TIP20

# HD derivation is gated behind an "unaudited" flag in eth_account.
Account.enable_unaudited_hdwallet_features()


def derivation_path(global_seq: int, index: int) -> str:
    """Mirror of the Go generator's path; index 0 is the validator key."""
    return f"m/44'/60'/{global_seq}'/0/{index}"


def derive_private_key(mnemonic: str, global_seq: int, index: int) -> str:
    account = Account.from_mnemonic(mnemonic.strip(), account_path=derivation_path(global_seq, index))
    return account.key.hex()


# Workload shapes. They differ in what the transaction touches, which is what
# separates "parallel execution flatters this" from "this contends":
#
#   self      - each sender transfers to itself: no shared storage between
#               senders, so execution can parallelise freely (the default).
#   hot       - every sender transfers to ONE recipient, whose balance is a
#               single hot storage slot: the contended counterpart to `self`,
#               and the honest workload for judging parallel execution.
#   noop      - zero-value self-call carrying no data: the floor, measuring
#               consensus and plumbing rather than execution.
#   batch     - several transfers batched into one transaction via the native
#               envelope's calls[] array. Note this changes what "a
#               transaction" means: report calls/s alongside tx/s.
#   fresh     - each transfer credits a never-seen recipient, so its balance
#               slot goes 0 -> non-zero: ~2x the gas (state creation) and the
#               only no-setup shape on the non-commutative storage path.
#   multitoken- round-robins the four genesis TIP-20s (PATH/ALPHA/BETA/THETA),
#               each a distinct storage tree, to spread trie contention.
#   approve   - sets an allowance, which writes the EXACT-checked allowance slot
#               (not the commutative balance path). This is the only shape that
#               exercises the storage action that can conflict under Tempo's
#               optimistic parallel execution.
#   memo      - transferWithMemo: a payment-lane transfer carrying an extra
#               32-byte memo, a distinct selector and calldata.
#   approve_transfer - approve then transferFrom in one tx: touches the
#               allowance slot twice (create + consume) plus two balances,
#               self-contained (the sender approves and pulls from itself).
TX_SHAPES = (
    "self",
    "hot",
    "noop",
    "batch",
    "fresh",
    "multitoken",
    "approve",
    "memo",
    "approve_transfer",
)

# A fixed 32-byte memo for the `memo` shape; its content is irrelevant.
MEMO = b"\x00" * 31 + b"\x01"

# The four TIP-20 tokens tempo-xtask mints to every derived account at genesis
# (unless generated with --no-extra-tokens). PATH is the fee token.
GENESIS_TOKENS = (
    "0x20c0000000000000000000000000000000000000",
    "0x20c0000000000000000000000000000000000001",
    "0x20c0000000000000000000000000000000000002",
    "0x20c0000000000000000000000000000000000003",
)

# Fixed recipient for the `hot` shape: derived from the mnemonic like every
# other account, so it exists and is funded, but never sends.
HOT_RECIPIENT_INDEX = 0


def fixed_recipient(args, signer):
    """The recipient shared by every transaction of an account, or None when it
    must vary per transaction (`fresh` credits a new address each time)."""
    if args.tx_shape == "hot":
        # A single funded address every sender writes to: the contended slot.
        return Account.from_mnemonic(
            args.mnemonic.strip(),
            account_path=derivation_path(args.global_seq, HOT_RECIPIENT_INDEX),
        ).address
    if args.tx_shape == "fresh":
        return None
    # Self-transfer: the amount is irrelevant and senders stay independent.
    return signer.checksum_address


def build_calls(args, recipient):
    """The calls[] for one transaction sending to `recipient`."""
    if args.tx_shape == "noop":
        # No data and no value: the cheapest transaction the chain accepts.
        return [dict(to=recipient, value=0, data=b"")]
    if args.tx_shape == "multitoken":
        # One call per genesis token: disjoint storage trees, identical gas.
        return [dict(to=t, value=0, data=TIP20.fns.transfer(recipient, 1).data) for t in GENESIS_TOKENS]
    if args.tx_shape == "approve":
        return [dict(to=args.token, value=0, data=TIP20.fns.approve(recipient, 1).data)]
    if args.tx_shape == "memo":
        return [dict(to=args.token, value=0, data=TIP20.fns.transferWithMemo(recipient, 1, MEMO).data)]
    if args.tx_shape == "approve_transfer":
        # Self-contained: the sender approves itself, then pulls from itself, so
        # the allowance slot is created and consumed within one transaction.
        return [
            dict(to=args.token, value=0, data=TIP20.fns.approve(recipient, 1).data),
            dict(to=args.token, value=0, data=TIP20.fns.transferFrom(recipient, recipient, 1).data),
        ]
    calls = [dict(to=args.token, value=0, data=TIP20.fns.transfer(recipient, 1).data)]
    if args.tx_shape == "batch":
        calls *= args.batch_calls
    return calls


def build_account_txs(job: tuple) -> list[str]:
    """Build and sign every transaction for one account.

    Runs in a worker process: signing is CPU-bound, and one account's
    transactions are a natural unit because their nonces are sequential.
    Takes the parsed arguments plus this account's index, since everything
    except the index is identical for every account.
    """
    args, index = job
    signer = Signer(derive_private_key(args.mnemonic, args.global_seq, index))
    # The sender pays gas in the fee token, so its balance drifts down over the
    # run; senders start with 2**64-1 units, which is ample.
    recipient = fixed_recipient(args, signer)

    raws = []
    for nonce in range(args.txs_per_account):
        builder = (
            Builder()
            .chain_id(args.chain_id)
            .gas_limit(args.gas_limit)
            .max_fee_per_gas(args.max_fee_per_gas)
            .max_priority_fee_per_gas(args.max_priority_fee_per_gas)
            .nonce(nonce)
            .nonce_key(args.nonce_key)
            .fee_token(args.fee_token or args.token)
        )
        # `fresh` gets a new recipient per transaction; every other shape reuses one.
        for call in build_calls(args, recipient or Account.create().address):
            builder = builder.add_call(**call)
        raws.append(serialize(sign_transaction(builder.build(), signer)))
    return raws


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", required=True, help="path of the JSON array to write")
    parser.add_argument("--global-seq", type=int, default=0, help="key derivation branch (m/44'/60'/<branch>'/0/i)")
    parser.add_argument(
        "--account-offset",
        type=int,
        default=0,
        help="skip this many accounts before the node's first one; lets several nodes draw"
        " disjoint senders from the same funded branch",
    )
    parser.add_argument("--accounts", type=int, required=True)
    parser.add_argument("--txs-per-account", type=int, required=True)
    parser.add_argument("--chain-id", type=int, required=True)
    parser.add_argument("--mnemonic", required=True)
    parser.add_argument("--token", default="0x20c0000000000000000000000000000000000000", help="TIP-20 transfer target")
    parser.add_argument("--fee-token", default="", help="token gas is paid in; defaults to --token")
    parser.add_argument("--gas-limit", type=int, default=300000)
    parser.add_argument("--max-fee-per-gas", type=int, required=True)
    parser.add_argument("--max-priority-fee-per-gas", type=int, default=0)
    parser.add_argument(
        "--nonce-key",
        type=int,
        default=0,
        help="nonce lane; one lane per account keeps ordering comparable with legacy txs",
    )
    parser.add_argument(
        "--tx-shape",
        choices=TX_SHAPES,
        default="self",
        help="workload shape; see TX_SHAPES for what each one contends on",
    )
    parser.add_argument("--batch-calls", type=int, default=4, help="calls per tx when --tx-shape=batch")
    parser.add_argument("--workers", type=int, default=0, help="signing processes (0 = cpu_count)")
    args = parser.parse_args()

    # A silently-empty run would look like a successful 0-TPS benchmark.
    if args.accounts < 1 or args.txs_per_account < 1:
        parser.error(f"--accounts and --txs-per-account must be >= 1 (got {args.accounts}, {args.txs_per_account})")
    # Every node would reject these, after signing the whole batch.
    if args.tx_shape == "batch" and args.batch_calls < 1:
        parser.error(f"--batch-calls must be >= 1 (got {args.batch_calls})")
    if args.max_priority_fee_per_gas > args.max_fee_per_gas:
        parser.error(
            f"--max-priority-fee-per-gas ({args.max_priority_fee_per_gas}) exceeds"
            f" --max-fee-per-gas ({args.max_fee_per_gas})"
        )
    # Fail before spending minutes signing into an unwritable location.
    out_dir = os.path.dirname(os.path.abspath(args.out)) or "."
    os.makedirs(out_dir, exist_ok=True)
    if not os.access(out_dir, os.W_OK):
        parser.error(f"--out directory is not writable: {out_dir}")

    # index 0 is the validator key, so benchmark accounts start at 1 — matching
    # bench.signAccountTxs — offset so each node takes a disjoint slice.
    jobs = [(args, args.account_offset + i + 1) for i in range(args.accounts)]

    started = time.perf_counter()
    workers = args.workers or (os.cpu_count() or 1)
    if workers > 1 and len(jobs) > 1:
        with ProcessPoolExecutor(max_workers=workers) as pool:
            # Ordered results keep the per-account grouping the Go generator produces.
            batches = list(pool.map(build_account_txs, jobs))
    else:
        batches = [build_account_txs(job) for job in jobs]

    raws = [raw for batch in batches for raw in batch]

    # Write via a temporary file so a crash cannot leave truncated JSON where a
    # previously valid transaction file used to be.
    tmp_path = f"{args.out}.tmp"
    with open(tmp_path, "w") as handle:
        json.dump(raws, handle)
    os.replace(tmp_path, args.out)

    elapsed = time.perf_counter() - started
    rate = len(raws) / elapsed if elapsed > 0 else 0.0
    # Consumed by the Go activity for its log line.
    print(
        json.dumps(
            {
                "txs": len(raws),
                "shape": args.tx_shape,
                "accounts": args.accounts,
                "seconds": round(elapsed, 3),
                "tx_per_sec": round(rate, 1),
                "out": args.out,
            }
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
