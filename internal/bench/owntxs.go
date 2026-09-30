package bench

import (
	"encoding/json"
	"strings"
)

// TxSet holds the hashes of a run's own transactions, so that on a shared
// chain other users' transactions are neither counted nor mistaken for load.
// They are the hashes the node returned on submission: Tempo stores a 0x76
// signature's v as 27/28 rather than the 0/1 sent, so keccak of the sent bytes
// never matches.
type TxSet map[string]struct{}

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
