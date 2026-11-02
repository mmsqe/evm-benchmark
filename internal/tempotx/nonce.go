package tempotx

import (
	"slices"

	"github.com/ethereum/go-ethereum/common"
)

// NoncePrecompile holds the nonces of user nonce keys; key 0 is the account's
// own nonce, which it refuses to report.
var NoncePrecompile = common.HexToAddress("0x4E4F4E4345000000000000000000000000000000")

var selGetNonce = selector("getNonce(address,uint256)")

// GetNonce builds getNonce(owner, key) calldata.
func GetNonce(owner common.Address, key uint64) []byte {
	return slices.Concat(selGetNonce, padAddr(owner), padUint(key))
}
