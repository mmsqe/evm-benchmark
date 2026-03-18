package keygen

import (
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
	hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
)

// DeterministicKey returns a deterministic ECDSA key for (globalSeq, index).
// If baseMnemonic is empty, it falls back to the legacy seed scheme.
func DeterministicKey(globalSeq, index int, baseMnemonic string) (*ecdsa.PrivateKey, error) {
	if strings.TrimSpace(baseMnemonic) == "" {
		var raw [32]byte
		seed := (uint64(globalSeq+1) << 32) | uint64(index)
		binary.BigEndian.PutUint64(raw[24:], seed)
		return crypto.ToECDSA(raw[:])
	}

	wallet, err := hdwallet.NewFromMnemonic(strings.TrimSpace(baseMnemonic))
	if err != nil {
		return nil, fmt.Errorf("create hd wallet from mnemonic: %w", err)
	}

	path := hdwallet.MustParseDerivationPath(fmt.Sprintf("m/44'/60'/%d'/0/%d", globalSeq, index))
	account, err := wallet.Derive(path, false)
	if err != nil {
		return nil, fmt.Errorf("derive account path: %w", err)
	}

	key, err := wallet.PrivateKey(account)
	if err != nil {
		return nil, fmt.Errorf("extract private key: %w", err)
	}

	return key, nil
}
