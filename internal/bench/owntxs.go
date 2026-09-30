package bench

import (
	"encoding/json"
	"strings"
)

// TxSet holds the hashes of a run's own transactions, so that on a shared
// chain other users' transactions are neither counted nor mistaken for load.
type TxSet map[string]struct{}

// NewTxSet hashes raws with the chain's own hash function, so the set does
// not depend on what the node answers: a request that times out may still
// have been admitted, and its transactions are then found in blocks.
func NewTxSet(raws []string, hash func(raw string) string) TxSet {
	s := make(TxSet, len(raws))
	for _, raw := range raws {
		s.add(hash(raw))
	}
	return s
}

func (s TxSet) add(hashes ...string) {
	for _, h := range hashes {
		s[strings.ToLower(h)] = struct{}{}
	}
}

// Count reports how many of a block's transaction hashes are in the set.
func (s TxSet) Count(txs []json.RawMessage) int {
	n := 0
	for _, raw := range txs {
		var hash string
		if json.Unmarshal(raw, &hash) != nil {
			continue
		}
		if _, ok := s[strings.ToLower(hash)]; ok {
			n++
		}
	}
	return n
}
