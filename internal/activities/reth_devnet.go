package activities

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/mmsqe/evm-benchmark/internal/bench"
	"github.com/mmsqe/evm-benchmark/internal/messages"
)

// reth has no devnet generator, so the genesis and launchers are written here.

const (
	rethDefaultGasLimit = 30_000_000
	rethGenesisBaseFee  = 1_000_000_000
)

var (
	// rethProducerKey is the producer's fixed p2p key, so fullnodes know its
	// enode before it starts.
	rethProducerKey = crypto.ToECDSAUnsafe(crypto.Keccak256([]byte("evm-benchmark reth producer")))

	// rethTokenAddress is where the genesis deploys rethTokenCode for ERC-20
	// load that names no token of its own.
	rethTokenAddress = common.HexToAddress("0x2020202020202020202020202020202020202020")

	// rethTokenBalance is each sender's initial token balance.
	rethTokenBalance = new(big.Int).Lsh(big.NewInt(1), 128)
)

// rethTokenCode is a minimal ERC-20 runtime: transfer and balanceOf over a
// Solidity-layout balance mapping at slot 0, emitting Transfer. Any other
// selector or an insufficient balance reverts.
//
//	00 PUSH0 CALLDATALOAD PUSH1 e0 SHR          selector
//	05 DUP1 PUSH4 a9059cbb EQ PUSH1 1b JUMPI    transfer(address,uint256)
//	0f PUSH4 70a08231 EQ PUSH1 79 JUMPI         balanceOf(address)
//	18 PUSH0 DUP1 REVERT
//	1b JUMPDEST                                 transfer:
//	1c CALLER PUSH0 MSTORE PUSH1 40 PUSH0 KECCAK256           from slot
//	23 DUP1 SLOAD PUSH1 24 CALLDATALOAD DUP1 DUP3 LT          balance < amount
//	2b PUSH1 8a JUMPI SWAP1 SUB SWAP1 SSTORE                  debit from
//	32 PUSH1 04 CALLDATALOAD PUSH0 MSTORE PUSH1 40 PUSH0 KECCAK256
//	3b DUP1 SLOAD PUSH1 24 CALLDATALOAD ADD SWAP1 SSTORE      credit to
//	43 PUSH1 24 CALLDATALOAD PUSH0 MSTORE PUSH1 04 CALLDATALOAD CALLER
//	4c PUSH32 ddf252ad.. PUSH1 20 PUSH0 LOG3                  Transfer(from, to, amount)
//	71 PUSH1 01 PUSH0 MSTORE PUSH1 20 PUSH0 RETURN            true
//	79 JUMPDEST                                 balanceOf:
//	7a PUSH1 04 CALLDATALOAD PUSH0 MSTORE PUSH1 40 PUSH0 KECCAK256 SLOAD
//	84 PUSH0 MSTORE PUSH1 20 PUSH0 RETURN
//	8a JUMPDEST PUSH0 DUP1 REVERT               insufficient balance
//
// Memory word 1 is never written, so KECCAK256 over words 0-1 hashes an
// address with mapping slot 0.
var rethTokenCode = common.FromHex("0x" +
	"5f3560e01c8063a9059cbb14601b576370a08231146079575f80fd" +
	"5b335f5260405f208054602435808210608a5790039055" +
	"6004355f5260405f208054602435019055" +
	"6024355f5260043533" +
	"7fddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef" +
	"60205fa3" +
	"60015f5260205ff3" +
	"5b6004355f5260405f20545f5260205ff3" +
	"5b5f80fd")

// generateRethDevnet writes one directory per node under spec.DataDir/devnet,
// holding the genesis and a `run.sh` launcher; it doubles as the reth datadir.
func generateRethDevnet(spec messages.BenchmarkSpec, nodes []messages.NodeTarget) error {
	if _, err := resetDevnetDir(spec); err != nil {
		return err
	}
	genesis, err := rethGenesis(spec, nodes)
	if err != nil {
		return err
	}
	genesisBytes, err := json.MarshalIndent(genesis, "", "  ")
	if err != nil {
		return fmt.Errorf("encode genesis: %w", err)
	}
	return writeDevnetNodes(spec, nodes, genesisBytes, func(node messages.NodeTarget) (runScript, error) {
		return rethLauncher(spec, len(nodes), node)
	})
}

// rethGenesis activates every fork through Osaka at genesis, like reth's dev
// chain, and funds every account the signer uses, natively and, for ERC-20 load,
// in the deployed token. Amsterdam stays off: its blocks carry access lists,
// which move reth onto a different validation path. genesis_patch wins.
func rethGenesis(spec messages.BenchmarkSpec, nodes []messages.NodeTarget) (map[string]interface{}, error) {
	deployToken := spec.TxType == messages.ERC20TransferTx &&
		common.HexToAddress(spec.ERC20ContractAddress) == rethTokenAddress
	addrs, err := signerAddresses(spec, nodes)
	if err != nil {
		return nil, err
	}
	alloc := make(map[string]interface{}, len(addrs)+1)
	tokenStorage := map[string]interface{}{}
	for _, addr := range addrs {
		alloc[allocKey(addr)] = map[string]interface{}{"balance": devnetAccountBalance}
		if deployToken {
			tokenStorage[rethTokenBalanceSlot(addr).Hex()] = common.BigToHash(rethTokenBalance).Hex()
		}
	}
	if deployToken {
		// Seeded so the first credit to it costs the same as every later one.
		tokenStorage[rethTokenBalanceSlot(bench.HotRecipient).Hex()] = common.BigToHash(big.NewInt(1)).Hex()
		alloc[allocKey(rethTokenAddress)] = map[string]interface{}{
			"balance": "0x0",
			"code":    hexutil.Encode(rethTokenCode),
			"storage": tokenStorage,
		}
	}

	genesis := map[string]interface{}{
		"config": map[string]interface{}{
			"chainId":                       spec.EVMChainID,
			"homesteadBlock":                0,
			"eip150Block":                   0,
			"eip155Block":                   0,
			"eip158Block":                   0,
			"byzantiumBlock":                0,
			"constantinopleBlock":           0,
			"petersburgBlock":               0,
			"istanbulBlock":                 0,
			"berlinBlock":                   0,
			"londonBlock":                   0,
			"mergeNetsplitBlock":            0,
			"terminalTotalDifficulty":       0,
			"terminalTotalDifficultyPassed": true,
			"shanghaiTime":                  0,
			"cancunTime":                    0,
			"pragueTime":                    0,
			"osakaTime":                     0,
			"blobSchedule": map[string]interface{}{
				"cancun": map[string]interface{}{"target": 3, "max": 6, "baseFeeUpdateFraction": 3338477},
				"prague": map[string]interface{}{"target": 6, "max": 9, "baseFeeUpdateFraction": 5007716},
				"osaka":  map[string]interface{}{"target": 6, "max": 9, "baseFeeUpdateFraction": 5007716},
			},
		},
		"nonce":         "0x0",
		"timestamp":     "0x0",
		"extraData":     "0x",
		"gasLimit":      fmt.Sprintf("0x%x", cmp.Or(spec.RethGasLimit, rethDefaultGasLimit)),
		"difficulty":    "0x0",
		"mixHash":       common.Hash{}.Hex(),
		"coinbase":      common.Address{}.Hex(),
		"baseFeePerGas": fmt.Sprintf("0x%x", rethGenesisBaseFee),
		"alloc":         alloc,
	}
	return deepMergeJSON(genesis, spec.GenesisPatch), nil
}

// rethTokenBalanceSlot is owner's balance slot in rethTokenCode:
// keccak256(owner . 0).
func rethTokenBalanceSlot(owner common.Address) common.Hash {
	return crypto.Keccak256Hash(common.LeftPadBytes(owner.Bytes(), 32), make([]byte, 32))
}

// rethProducerPort returns a port from the producer's (node 0's) block.
func rethProducerPort(spec messages.BenchmarkSpec, offset int) int {
	return rethPorts.base(spec.RethBasePort, 0) + offset
}

func rethProducerEnode(spec messages.BenchmarkSpec) string {
	// An enode ID is the uncompressed public key without its 0x04 prefix.
	id := hex.EncodeToString(crypto.FromECDSAPub(&rethProducerKey.PublicKey)[1:])
	return fmt.Sprintf("enode://%s@127.0.0.1:%d", id, rethProducerPort(spec, rethP2PPortOffset))
}

// rethLauncher builds the node's `run.sh`. The validator is the dev block
// producer; every other node follows it as a trusted peer and through the debug
// consensus client. Paths are relative to the node dir, which the launcher cd's
// into.
func rethLauncher(spec messages.BenchmarkSpec, nodeCount int, node messages.NodeTarget) (runScript, error) {
	blockTime, err := rethBlockTime(spec)
	if err != nil {
		return runScript{}, err
	}
	base := rethPorts.base(spec.RethBasePort, node.GlobalSeq)
	args := []string{
		"node",
		"--chain", "./genesis.json",
		"--datadir", ".",
		"--log.file.directory", "./logs",
		"--http",
		"--http.addr", "127.0.0.1",
		"--http.port", strconv.Itoa(base + rethHTTPPortOffset),
		"--http.api", "eth,net,web3,txpool",
		"--authrpc.port", strconv.Itoa(base + rethAuthRPCPortOffset),
		"--addr", "127.0.0.1",
		"--port", strconv.Itoa(base + rethP2PPortOffset),
		// Peers come from the producer's enode; nothing is discovered and no
		// public IP is looked up.
		"--disable-discovery",
		"--nat", "none",
		"--metrics", fmt.Sprintf("127.0.0.1:%d", base+rethMetricsPortOffset),
		// An IPC socket would collide between node dirs.
		"--ipcdisable",
	}
	script := runScript{Binary: cmp.Or(spec.RethBin, rethDefaultNodeBin)}
	producer := node.Group == "validators"
	if producer {
		args = append(args,
			"--dev",
			// reth only parses whole-number spans, so Go's "1.5s" would fail.
			"--dev.block-time", fmt.Sprintf("%dms", blockTime.Milliseconds()),
			"--ws",
			"--ws.addr", "127.0.0.1",
			"--ws.port", strconv.Itoa(base+rethWSPortOffset),
			"--ws.api", "eth",
			"--p2p-secret-key-hex", hex.EncodeToString(crypto.FromECDSA(rethProducerKey)),
		)
		if spec.RethGasLimit > 0 {
			// Otherwise the builder walks the limit toward its own default.
			args = append(args, "--builder.gaslimit", strconv.FormatInt(spec.RethGasLimit, 10))
		}
	} else {
		wsPort := rethProducerPort(spec, rethWSPortOffset)
		args = append(args,
			"--trusted-peers", rethProducerEnode(spec),
			"--debug.rpc-consensus-url", fmt.Sprintf("ws://127.0.0.1:%d", wsPort),
		)
		// The debug consensus client exits if the producer, launched at the same
		// time, is not serving yet.
		script.Preamble = []string{
			fmt.Sprintf("until curl -s -o /dev/null 'http://127.0.0.1:%d'; do sleep 0.2; done", wsPort),
		}
	}
	// Gossip puts every node's load in every pool.
	poolSpec := spec
	poolSpec.NumAccounts *= nodeCount
	args = append(args, rethTxPoolArgs(poolSpec)...)
	// Extra flags last, so they override any default above.
	args = append(args, spec.RethNodeArgs...)
	if !producer {
		args = append(args, spec.RethFullnodeArgs...)
	}
	script.Args = args
	return script, nil
}
