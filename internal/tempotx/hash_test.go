package tempotx

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

// TestHashUsesStoredSignatureForm: the node hashes a 0x76 envelope with v as
// 27/28, so the hash of what SignedRaw emits (v in {0,1}) must be taken over
// that form — and be the same whichever form was sent.
func TestHashUsesStoredSignatureForm(t *testing.T) {
	sent := hexutil.MustDecode(refSelf) // ends in v=1
	stored := append(append([]byte{}, sent[:len(sent)-1]...), 28)
	want := crypto.Keccak256Hash(stored).Hex()

	if got := Hash(refSelf); got != want {
		t.Errorf("Hash(sent form) = %s, want %s", got, want)
	}
	if got := Hash(hexutil.Encode(stored)); got != want {
		t.Errorf("Hash(stored form) = %s, want %s", got, want)
	}
	if got := Hash(refSelf); got == crypto.Keccak256Hash(sent).Hex() {
		t.Error("hash taken over the sent bytes, which the node never files a transaction under")
	}
	legacy := "0x02f86b01018459682f00850df847580082520894000000000000000000000000000000000000dead0180c0"
	if got := Hash(legacy); !strings.EqualFold(got, crypto.Keccak256Hash(hexutil.MustDecode(legacy)).Hex()) {
		t.Errorf("non-0x76 transaction not hashed as sent: %s", got)
	}
}
