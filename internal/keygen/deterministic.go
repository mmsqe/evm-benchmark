package keygen

import (
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/crypto"
	hdwallet "github.com/miguelmota/go-ethereum-hdwallet"
)

// seeds caches each mnemonic's BIP-39 seed, about half a millisecond to stretch.
// The wallet is built per call, since one cannot be shared between goroutines.
var seeds sync.Map

// DeterministicKey returns a deterministic ECDSA key for (globalSeq, index).
// If baseMnemonic is empty, it falls back to the legacy seed scheme.
func DeterministicKey(globalSeq, index int, baseMnemonic string) (*ecdsa.PrivateKey, error) {
	baseMnemonic = strings.TrimSpace(baseMnemonic)
	if baseMnemonic == "" {
		var raw [32]byte
		seed := (uint64(globalSeq+1) << 32) | uint64(index)
		binary.BigEndian.PutUint64(raw[24:], seed)
		return crypto.ToECDSA(raw[:])
	}

	seed, err := seedFor(baseMnemonic)
	if err != nil {
		return nil, err
	}
	wallet, err := hdwallet.NewFromSeed(seed)
	if err != nil {
		return nil, fmt.Errorf("create hd wallet from seed: %w", err)
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

// seedFor returns the mnemonic's seed, stretching it on first use.
func seedFor(mnemonic string) ([]byte, error) {
	if seed, ok := seeds.Load(mnemonic); ok {
		return seed.([]byte), nil
	}
	seed, err := hdwallet.NewSeedFromMnemonic(mnemonic)
	if err != nil {
		return nil, fmt.Errorf("seed from mnemonic: %w", err)
	}
	cached, _ := seeds.LoadOrStore(mnemonic, seed)
	return cached.([]byte), nil
}
