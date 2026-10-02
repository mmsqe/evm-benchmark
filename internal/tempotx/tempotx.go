// Package tempotx builds and signs Tempo's native type-0x76 transaction
// envelope in Go for the benchmark's load generator. It covers the subset the
// benchmark uses: a sender-signed transaction with batched calls, a fee token
// and a 2D nonce, and no fee payer, access list or key authorization.
//
// The encoding is verified byte-for-byte against Tempo's canonical encoding in
// tempotx_test.go; go-ethereum signs secp256k1 with RFC-6979 deterministic
// nonces and low-s, so even the signature bytes are reproducible.
package tempotx

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// txTypeByte is the type prefix of the native envelope; the same byte is part
// of the payload hashed for signing.
const txTypeByte = 0x76

// Call is one call in a native transaction's batch: a target, a value and
// calldata. The benchmark always uses value 0.
type Call struct {
	To    common.Address
	Value uint64
	Data  []byte
}

// Tx is the subset of the 0x76 envelope the benchmark produces: sender-signed,
// no fee payer, access list or key authorization.
//
// FeeToken is always encoded (20 bytes): a zero value is the zero-address fee
// token, not an absent one. The benchmark always sets a real fee token, so the
// "no fee token" (native gas) case is intentionally unrepresented.
type Tx struct {
	ChainID              uint64
	MaxPriorityFeePerGas uint64
	MaxFeePerGas         uint64
	GasLimit             uint64
	NonceKey             uint64
	Nonce                uint64
	FeeToken             common.Address
	Calls                []Call
}

// SignedRaw builds, signs and serializes the transaction, returning the
// broadcast-ready "0x76…" hex string RunNode sends unchanged.
func (t *Tx) SignedRaw(key *ecdsa.PrivateKey) (string, error) {
	unsigned, err := rlp.EncodeToBytes(t.fields(nil))
	if err != nil {
		return "", fmt.Errorf("encode signing payload: %w", err)
	}
	// The sender signs keccak256(0x76 || rlp(fields-without-signature)).
	sigHash := crypto.Keccak256(append([]byte{txTypeByte}, unsigned...))
	sig, err := crypto.Sign(sigHash, key) // 65 bytes: r || s || v, v in {0,1}
	if err != nil {
		return "", fmt.Errorf("sign transaction: %w", err)
	}
	signed, err := rlp.EncodeToBytes(t.fields(sig))
	if err != nil {
		return "", fmt.Errorf("encode signed transaction: %w", err)
	}
	return "0x76" + hex.EncodeToString(signed), nil
}

// Hash returns the hash the chain files a raw 0x76 transaction under. The
// node stores the signature's v as 27/28 and hashes that form, so keccak of
// an envelope signed with v in {0,1} (as SignedRaw emits) never matches; the
// signature is the last field, so only the final byte differs. Verified on
// Moderato: an envelope sent with v=27 came back with exactly this hash.
// Other transaction types hash as sent.
func Hash(raw string) string {
	b, err := hexutil.Decode(raw)
	if err != nil {
		return ""
	}
	if len(b) > 1 && b[0] == txTypeByte && b[len(b)-1] < 27 {
		b = append(append([]byte{}, b[:len(b)-1]...), b[len(b)-1]+27)
	}
	return crypto.Keccak256Hash(b).Hex()
}

// fields builds the RLP field list. sig==nil yields the sender-signing payload
// (13 fields); a non-nil signature appends field 13 for broadcast. This is the
// field layout for the no-fee-payer case.
func (t *Tx) fields(sig []byte) []interface{} {
	calls := make([]interface{}, len(t.Calls))
	for i, c := range t.Calls {
		calls[i] = []interface{}{c.To.Bytes(), c.Value, c.Data}
	}
	fields := []interface{}{
		t.ChainID,              // 0: chainId
		t.MaxPriorityFeePerGas, // 1: maxPriorityFeePerGas
		t.MaxFeePerGas,         // 2: maxFeePerGas
		t.GasLimit,             // 3: gas
		calls,                  // 4: calls [[to, value, data], ...]
		[]interface{}{},        // 5: accessList (empty)
		t.NonceKey,             // 6: nonceKey
		t.Nonce,                // 7: nonce
		uint64(0),              // 8: validBefore
		uint64(0),              // 9: validAfter
		t.FeeToken.Bytes(),     // 10: feeToken (20 bytes)
		[]byte{},               // 11: feePayerSignatureOrSender (none)
		[]interface{}{},        // 12: authorizationList (empty)
	}
	if sig != nil {
		fields = append(fields, sig) // 13: sender signature envelope (65 bytes)
	}
	return fields
}
