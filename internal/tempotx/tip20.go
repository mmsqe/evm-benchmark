package tempotx

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TIP-20 calldata builders. Every function used by the benchmark takes only
// static ABI types (address, uint256, bytes32), so encoding is the 4-byte
// selector followed by 32-byte left-padded arguments — no head/tail packing.
// Selectors are derived from the canonical signatures rather than hard-coded.

func selector(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

var (
	selTransfer         = selector("transfer(address,uint256)")
	selApprove          = selector("approve(address,uint256)")
	selTransferFrom     = selector("transferFrom(address,address,uint256)")
	selTransferWithMemo = selector("transferWithMemo(address,uint256,bytes32)")
)

func padAddr(a common.Address) []byte { return common.LeftPadBytes(a.Bytes(), 32) }

func padUint(v uint64) []byte {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(v)
		v >>= 8
	}
	return common.LeftPadBytes(b[:], 32)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// Transfer builds transfer(to, amount) calldata.
func Transfer(to common.Address, amount uint64) []byte {
	return concat(selTransfer, padAddr(to), padUint(amount))
}

// Approve builds approve(spender, amount) calldata.
func Approve(spender common.Address, amount uint64) []byte {
	return concat(selApprove, padAddr(spender), padUint(amount))
}

// TransferFrom builds transferFrom(sender, to, amount) calldata.
func TransferFrom(sender, to common.Address, amount uint64) []byte {
	return concat(selTransferFrom, padAddr(sender), padAddr(to), padUint(amount))
}

// TransferWithMemo builds transferWithMemo(to, amount, memo) calldata.
func TransferWithMemo(to common.Address, amount uint64, memo [32]byte) []byte {
	return concat(selTransferWithMemo, padAddr(to), padUint(amount), memo[:])
}
