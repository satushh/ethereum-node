// Command devnet-spam floods a devnet execution node with self-transfers so
// benchmark blocks are non-empty. Devnet only: the default key is publicly
// known and must be prefunded in the genesis alloc.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// The classic prysm devnet-guide key; address 0x123463a4B065722E99115D6c222f267d9cABb524.
const defaultKey = "2e0834786285daccd064ca17f1654f67b4aef298acbb82cef9ec422fb4975622"

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "execution JSON-RPC endpoint")
	keyHex := flag.String("key", defaultKey, "hex private key of a prefunded account")
	rate := flag.Int("rate", 50, "transactions per second")
	flag.Parse()
	if err := run(*rpcURL, *keyHex, *rate); err != nil {
		fmt.Fprintln(os.Stderr, "devnet-spam:", err)
		os.Exit(1)
	}
}

func run(rpcURL, keyHex string, rate int) error {
	key, err := crypto.HexToECDSA(keyHex)
	if err != nil {
		return err
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return err
	}
	chainID, err := client.ChainID(ctx)
	if err != nil {
		return err
	}
	nonce, err := client.PendingNonceAt(ctx, addr)
	if err != nil {
		return err
	}
	signer := types.LatestSignerForChainID(chainID)
	fmt.Printf("devnet-spam: %s on chain %s at %d tx/s\n", addr.Hex(), chainID, rate)

	sent := 0
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("devnet-spam: done, %d txs sent\n", sent)
			return nil
		case <-tick.C:
			for i := 0; i < rate; i++ {
				tx := types.MustSignNewTx(key, signer, &types.DynamicFeeTx{
					ChainID:   chainID,
					Nonce:     nonce,
					To:        &addr, // self-transfer
					Value:     big.NewInt(1),
					Gas:       21000,
					GasTipCap: big.NewInt(1_000_000_000),
					GasFeeCap: big.NewInt(100_000_000_000),
				})
				if err := client.SendTransaction(ctx, tx); err != nil {
					// nonce drift (restart, reorg): resync and continue
					if n, nerr := client.PendingNonceAt(ctx, addr); nerr == nil {
						nonce = n
					}
					continue
				}
				nonce++
				sent++
			}
			if sent%500 < rate {
				fmt.Printf("devnet-spam: %d txs sent\n", sent)
			}
		}
	}
}
