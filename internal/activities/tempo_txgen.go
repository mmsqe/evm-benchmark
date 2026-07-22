package activities

import (
	"context"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/keygen"
	"github.com/mmsqe/evm-benchmark/internal/messages"
	"github.com/mmsqe/evm-benchmark/internal/tempotx"
)

// Native Tempo (0x76) transaction generation. Keys are derived exactly as
// internal/keygen does, so a chain funded for the built-in signer is funded for
// this one; the envelope encoding is the byte-verified one in internal/tempotx.

// hotRecipientIndex is the single funded address every sender writes to in the
// `hot` shape: derived like every other account (so it exists and is funded)
// but never sends.
const hotRecipientIndex = 0

// tempoGenesisTokens are the four TIP-20s tempo-xtask mints to every derived
// account at genesis; the `multitoken` shape round-robins them.
var tempoGenesisTokens = []common.Address{
	common.HexToAddress("0x20c0000000000000000000000000000000000000"),
	common.HexToAddress("0x20c0000000000000000000000000000000000001"),
	common.HexToAddress("0x20c0000000000000000000000000000000000002"),
	common.HexToAddress("0x20c0000000000000000000000000000000000003"),
}

// tempoMemo is the fixed 32-byte memo for the `memo` shape; its content is
// irrelevant (31 zero bytes then 0x01).
var tempoMemo = func() [32]byte {
	var m [32]byte
	m[31] = 0x01
	return m
}()

// tempoTxShapes is the set of workload shapes the generator accepts.
var tempoTxShapes = map[string]bool{
	"self": true, "hot": true, "noop": true, "batch": true, "fresh": true,
	"multitoken": true, "approve": true, "memo": true, "approve_transfer": true,
}

// generateTempoNativeTxs derives this node's accounts and signs NumTxs native
// transactions each, returning the raw hex strings in per-account order.
// Signing is parallelised across accounts.
func generateTempoNativeTxs(ctx context.Context, spec messages.BenchmarkSpec, target messages.NodeTarget) ([]string, error) {
	if spec.NumAccounts < 1 || spec.NumTxs < 1 {
		return nil, fmt.Errorf("num_accounts and num_txs must be >= 1 (got %d, %d)", spec.NumAccounts, spec.NumTxs)
	}
	shape := spec.TempoTxShape
	if shape == "" {
		shape = "self"
	}
	if !tempoTxShapes[shape] {
		return nil, fmt.Errorf("unknown tempo_tx_shape %q", shape)
	}
	batchCalls := spec.TempoBatchCalls
	if batchCalls <= 0 {
		batchCalls = 4
	}
	if shape == "batch" && batchCalls < 1 {
		return nil, fmt.Errorf("tempo_batch_calls must be >= 1 (got %d)", batchCalls)
	}
	lanes := spec.TempoNonceLanes
	if lanes < 1 {
		lanes = 1
	}

	token := common.HexToAddress(tempoDefaultFeeToken)
	if spec.ERC20ContractAddress != "" {
		token = common.HexToAddress(spec.ERC20ContractAddress)
	}
	// Gas is always paid in the genesis fee token: a custom transfer target is
	// not necessarily a valid or funded fee token.
	feeToken := common.HexToAddress(tempoDefaultFeeToken)

	// The hot shape's shared recipient, resolved once.
	var hotRecipient common.Address
	if shape == "hot" {
		key, err := keygen.DeterministicKey(0, hotRecipientIndex, spec.BaseMnemonic)
		if err != nil {
			return nil, fmt.Errorf("derive hot recipient: %w", err)
		}
		hotRecipient = crypto.PubkeyToAddress(key.PublicKey)
	}

	// tempo-xtask funds only the m/44'/60'/0'/0/i branch, so every node draws
	// from that branch and takes a disjoint slice via the offset, rather than
	// using its own (unfunded) branch the way the legacy signer does. Index 0 is
	// the validator key, so accounts start at offset+1.
	accountOffset := target.GlobalSeq * spec.NumAccounts

	signAccount := func(accountIdx int) ([]string, error) {
		key, err := keygen.DeterministicKey(0, accountIdx, spec.BaseMnemonic)
		if err != nil {
			return nil, fmt.Errorf("derive account %d: %w", accountIdx, err)
		}
		self := crypto.PubkeyToAddress(key.PublicKey)

		// The account's fixed recipient: hot writes one shared slot, fresh varies
		// per transaction, everything else self-transfers.
		recipient := self
		if shape == "hot" {
			recipient = hotRecipient
		}

		// Spread the account's transactions round-robin across `lanes` 2D-nonce
		// lanes (nonce_key = base + lane), each lane with its own sequential
		// nonce. One lane (the default) reproduces plain sequential nonces; many
		// lanes let a single owner issue parallel-eligible transactions — with
		// tx_shape=approve they all write the same exact-checked allowance slot,
		// which is the workload that can conflict under optimistic execution.
		laneNonce := make([]uint64, lanes)
		raws := make([]string, spec.NumTxs)
		for i := 0; i < spec.NumTxs; i++ {
			r := recipient
			if shape == "fresh" {
				fresh, err := crypto.GenerateKey()
				if err != nil {
					return nil, fmt.Errorf("generate fresh recipient: %w", err)
				}
				r = crypto.PubkeyToAddress(fresh.PublicKey)
			}
			lane := i % lanes
			tx := &tempotx.Tx{
				ChainID:              uint64(spec.EVMChainID),
				MaxPriorityFeePerGas: uint64(spec.TempoMaxPriorityFeePerGas),
				MaxFeePerGas:         uint64(spec.GasPriceWei),
				GasLimit:             spec.ERC20TransferGas,
				NonceKey:             uint64(spec.TempoNonceKey) + uint64(lane),
				Nonce:                laneNonce[lane],
				FeeToken:             feeToken,
				Calls:                buildTempoCalls(shape, token, r, batchCalls),
			}
			raw, err := tx.SignedRaw(key)
			if err != nil {
				return nil, fmt.Errorf("sign account %d tx %d: %w", accountIdx, i, err)
			}
			raws[i] = raw
			laneNonce[lane]++
		}
		return raws, nil
	}

	batches := make([][]string, spec.NumAccounts)
	jobs := make(chan int)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	workers := tempoSigningWorkers(spec)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				raws, err := signAccount(accountOffset + i + 1)
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					continue
				}
				batches[i] = raws
			}
		}()
	}
feed:
	for i := 0; i < spec.NumAccounts; i++ {
		select {
		case jobs <- i:
		case <-ctx.Done():
			mu.Lock()
			if firstErr == nil {
				firstErr = ctx.Err()
			}
			mu.Unlock()
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	raws := make([]string, 0, spec.NumAccounts*spec.NumTxs)
	for _, batch := range batches {
		raws = append(raws, batch...)
	}
	return raws, nil
}

// buildTempoCalls returns the calls[] for one transaction of the given shape.
func buildTempoCalls(shape string, token, recipient common.Address, batchCalls int) []tempotx.Call {
	switch shape {
	case "noop":
		// No data and no value: the cheapest transaction the chain accepts.
		return []tempotx.Call{{To: recipient}}
	case "multitoken":
		// One transfer per genesis token: disjoint storage trees, identical gas.
		calls := make([]tempotx.Call, len(tempoGenesisTokens))
		for i, tok := range tempoGenesisTokens {
			calls[i] = tempotx.Call{To: tok, Data: tempotx.Transfer(recipient, 1)}
		}
		return calls
	case "approve":
		return []tempotx.Call{{To: token, Data: tempotx.Approve(recipient, 1)}}
	case "memo":
		return []tempotx.Call{{To: token, Data: tempotx.TransferWithMemo(recipient, 1, tempoMemo)}}
	case "approve_transfer":
		// Self-contained: approve then pull from itself, so the allowance slot is
		// created and consumed within one transaction.
		return []tempotx.Call{
			{To: token, Data: tempotx.Approve(recipient, 1)},
			{To: token, Data: tempotx.TransferFrom(recipient, recipient, 1)},
		}
	default: // self, hot, fresh, batch
		calls := []tempotx.Call{{To: token, Data: tempotx.Transfer(recipient, 1)}}
		if shape == "batch" {
			batched := make([]tempotx.Call, batchCalls)
			for i := range batched {
				batched[i] = calls[0]
			}
			return batched
		}
		return calls
	}
}
