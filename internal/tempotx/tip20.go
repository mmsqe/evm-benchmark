package tempotx

import (
	"encoding/binary"
	"slices"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// TIP-20 calldata builders. Every function used by the benchmark takes only
// static ABI types (address, uint256, bytes32), so encoding is the 4-byte
// selector followed by 32-byte left-padded arguments — no head/tail packing.
// Selectors are derived from the canonical signatures rather than hard-coded.

// GenesisTokens are the four TIP-20s every Tempo genesis mints to its funded
// accounts: pathUSD, which is also the fee token, then AlphaUSD, BetaUSD and
// ThetaUSD.
var GenesisTokens = []common.Address{
	common.HexToAddress("0x20c0000000000000000000000000000000000000"),
	common.HexToAddress("0x20c0000000000000000000000000000000000001"),
	common.HexToAddress("0x20c0000000000000000000000000000000000002"),
	common.HexToAddress("0x20c0000000000000000000000000000000000003"),
}

// FeeToken is the token gas is paid in, present in every Tempo genesis.
var FeeToken = GenesisTokens[0]

func selector(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

var (
	selTransfer         = selector("transfer(address,uint256)")
	selApprove          = selector("approve(address,uint256)")
	selTransferFrom     = selector("transferFrom(address,address,uint256)")
	selTransferWithMemo = selector("transferWithMemo(address,uint256,bytes32)")
)

func padAddr(a common.Address) []byte { return common.LeftPadBytes(a.Bytes(), 32) }

func padUint(v uint64) []byte { return common.LeftPadBytes(binary.BigEndian.AppendUint64(nil, v), 32) }

// Transfer builds transfer(to, amount) calldata.
func Transfer(to common.Address, amount uint64) []byte {
	return slices.Concat(selTransfer, padAddr(to), padUint(amount))
}

// Approve builds approve(spender, amount) calldata.
func Approve(spender common.Address, amount uint64) []byte {
	return slices.Concat(selApprove, padAddr(spender), padUint(amount))
}

// TransferFrom builds transferFrom(sender, to, amount) calldata.
func TransferFrom(sender, to common.Address, amount uint64) []byte {
	return slices.Concat(selTransferFrom, padAddr(sender), padAddr(to), padUint(amount))
}

// TransferWithMemo builds transferWithMemo(to, amount, memo) calldata.
func TransferWithMemo(to common.Address, amount uint64, memo [32]byte) []byte {
	return slices.Concat(selTransferWithMemo, padAddr(to), padUint(amount), memo[:])
}
