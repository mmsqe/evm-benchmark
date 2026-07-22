package tempotx

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
)

// StablecoinDEX calldata builders for the order-book DEX at 0xDEc0…0000.
// Static ABI types only (address, uint128, bool, int16), so encoding is the
// 4-byte selector plus 32-byte words. Ticks are signed int16s, so negatives
// are two's-complement sign-extended to 256 bits.

// StablecoinDEXAddress is the order-book DEX precompile.
var StablecoinDEXAddress = common.HexToAddress("0xDEc0000000000000000000000000000000000000")

var (
	selPlaceFlip         = selector("placeFlip(address,uint128,bool,int16,int16)")
	selSwapExactAmountIn = selector("swapExactAmountIn(address,address,uint128,uint128)")
)

func padBool(b bool) []byte {
	out := make([]byte, 32)
	if b {
		out[31] = 1
	}
	return out
}

// padInt encodes a signed integer as a 32-byte two's-complement word,
// sign-extending negatives (0xFF… high bytes).
func padInt(v int64) []byte {
	out := make([]byte, 32)
	if v < 0 {
		for i := range out {
			out[i] = 0xFF
		}
	}
	binary.BigEndian.PutUint64(out[24:], uint64(v))
	return out
}

// PlaceFlip builds placeFlip(token, amount, isBid, tick, flipTick) calldata: a
// resting order at `tick` that flips to `flipTick` when filled — the order-book
// liquidity primitive tempo's own bench uses to build bid/ask walls.
func PlaceFlip(token common.Address, amount uint64, isBid bool, tick, flipTick int16) []byte {
	return concat(selPlaceFlip, padAddr(token), padUint(amount), padBool(isBid),
		padInt(int64(tick)), padInt(int64(flipTick)))
}

// SwapExactAmountIn builds swapExactAmountIn(tokenIn, tokenOut, amountIn,
// minAmountOut) calldata, matched against resting orders.
func SwapExactAmountIn(tokenIn, tokenOut common.Address, amountIn, minAmountOut uint64) []byte {
	return concat(selSwapExactAmountIn, padAddr(tokenIn), padAddr(tokenOut),
		padUint(amountIn), padUint(minAmountOut))
}
