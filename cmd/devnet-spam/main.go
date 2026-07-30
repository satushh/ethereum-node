// Command devnet-spam keeps a devnet's txpool busy: it sends self-transfers
// from N deterministic dev accounts (private keys 0x...01, 0x...02, ...,
// publicly known — devnet only). The same accounts must be prefunded in the
// devnet genesis; scripts/devnet-up.sh does both halves.
package main

import (
	"context"
	"crypto/ecdsa"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

func devKey(i int) *ecdsa.PrivateKey {
	k, err := crypto.HexToECDSA(fmt.Sprintf("%064x", i))
	if err != nil {
		panic(err) // unreachable for small positive i
	}
	return k
}

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "execution JSON-RPC endpoint")
	rate := flag.Int("rate", 30, "total transactions per second across all accounts")
	numAccounts := flag.Int("num-accounts", 3, "number of deterministic dev accounts")
	printAddrs := flag.Bool("print-addrs", false, "print the account addresses and exit (for genesis funding)")
	flag.Parse()

	if *printAddrs {
		for i := 1; i <= *numAccounts; i++ {
			fmt.Println(crypto.PubkeyToAddress(devKey(i).PublicKey).Hex())
		}
		return
	}
	if err := run(*rpcURL, *rate, *numAccounts); err != nil {
		fmt.Fprintln(os.Stderr, "devnet-spam:", err)
		os.Exit(1)
	}
}

func run(rpcURL string, rate, numAccounts int) error {
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
	signer := types.LatestSignerForChainID(chainID)

	var sent atomic.Int64
	perAccount := max(rate/numAccounts, 1)
	for i := 1; i <= numAccounts; i++ {
		key := devKey(i)
		addr := crypto.PubkeyToAddress(key.PublicKey)
		nonce, err := client.PendingNonceAt(ctx, addr)
		if err != nil {
			return fmt.Errorf("nonce for %s: %w", addr.Hex(), err)
		}
		fmt.Printf("devnet-spam: %s at %d tx/s\n", addr.Hex(), perAccount)
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					for range perAccount {
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
							if n, nerr := client.PendingNonceAt(ctx, addr); nerr == nil {
								nonce = n // resync after restarts/reorgs
							}
							continue
						}
						nonce++
						sent.Add(1)
					}
				}
			}
		}()
	}

	report := time.NewTicker(30 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("devnet-spam: done, %d txs sent\n", sent.Load())
			return nil
		case <-report.C:
			fmt.Printf("devnet-spam: %d txs sent\n", sent.Load())
		}
	}
}
