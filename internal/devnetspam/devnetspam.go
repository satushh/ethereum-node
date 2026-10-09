// Package devnetspam keeps a devnet's txpool busy: it sends self-transfers
// from N deterministic dev accounts (private keys 0x...01, 0x...02, ...,
// publicly known — devnet only). The same accounts must be prefunded in the
// devnet genesis; Addresses feeds that funding step. Used by both
// cmd/devnet-spam and `ethereum-node devnet`.
package devnetspam

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync/atomic"
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

// Addresses returns the hex addresses of the first n deterministic dev
// accounts, for genesis funding.
func Addresses(n int) []string {
	addrs := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		addrs = append(addrs, crypto.PubkeyToAddress(devKey(i).PublicKey).Hex())
	}
	return addrs
}

// Run spams self-transfers until ctx is canceled. It blocks; cancellation is
// the only clean exit (returns nil).
func Run(ctx context.Context, rpcURL string, rate, numAccounts int) error {
	if rate <= 0 || numAccounts <= 0 {
		return fmt.Errorf("rate (%d) and num-accounts (%d) must be positive", rate, numAccounts)
	}
	client, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return err
	}
	defer client.Close()
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
