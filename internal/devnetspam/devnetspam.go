// Package devnetspam keeps a devnet's txpool busy: it sends self-transfers
// from N deterministic dev accounts (private keys 0x...01, 0x...02, ...,
// publicly known; devnet only), plus type-3 blob transactions from the last
// of those accounts. The blob sender needs a dedicated account because geth's
// subpools reserve a sender exclusively: a pending blob tx blocks plain txs
// from the same account (ErrAlreadyReserved) and vice versa. The same
// accounts must be prefunded in the devnet genesis; Addresses feeds that
// funding step. Used by both cmd/devnet-spam and `ethereum-node devnet`.
package devnetspam

import (
	"context"
	"crypto/ecdsa"
	"encoding/binary"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/holiman/uint256"
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

// Run spams self-transfers (plus blob transactions when blobPeriod > 0)
// until ctx is canceled. It blocks; cancellation is the only clean exit
// (returns nil).
func Run(ctx context.Context, rpcURL string, rate, numAccounts int, blobPeriod time.Duration) error {
	if rate <= 0 || numAccounts <= 0 {
		return fmt.Errorf("rate (%d) and num-accounts (%d) must be positive", rate, numAccounts)
	}
	plainAccounts := numAccounts
	if blobPeriod > 0 {
		plainAccounts-- // the last account goes blob-only (subpool reservation)
		if plainAccounts == 0 {
			return fmt.Errorf("blob spam needs num-accounts >= 2 (one account becomes blob-only), got %d", numAccounts)
		}
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

	var sent, blobTxsSent atomic.Int64
	perAccount := max(rate/plainAccounts, 1)
	for i := 1; i <= plainAccounts; i++ {
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

	if blobPeriod > 0 {
		key := devKey(numAccounts)
		addr := crypto.PubkeyToAddress(key.PublicKey)
		nonce, err := client.PendingNonceAt(ctx, addr)
		if err != nil {
			return fmt.Errorf("nonce for %s: %w", addr.Hex(), err)
		}
		fmt.Printf("devnet-spam: %s sending a 1-blob tx every %s\n", addr.Hex(), blobPeriod)
		go func() {
			tick := time.NewTicker(blobPeriod)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					tx, err := blobTx(key, signer, chainID, addr, nonce)
					if err != nil {
						fmt.Printf("devnet-spam: blob tx build failed: %v\n", err)
						continue
					}
					if err := client.SendTransaction(ctx, tx); err != nil {
						// low-rate loop: errors are worth printing, unlike
						// the plain loop's 30/s
						fmt.Printf("devnet-spam: blob tx send failed: %v\n", err)
						if n, nerr := client.PendingNonceAt(ctx, addr); nerr == nil {
							nonce = n
						}
						continue
					}
					nonce++
					blobTxsSent.Add(1)
				}
			}
		}()
	}

	report := time.NewTicker(30 * time.Second)
	defer report.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Printf("devnet-spam: done, %d txs sent (%d blob txs)\n", sent.Load()+blobTxsSent.Load(), blobTxsSent.Load())
			return nil
		case <-report.C:
			fmt.Printf("devnet-spam: %d txs sent (%d blob txs)\n", sent.Load()+blobTxsSent.Load(), blobTxsSent.Load())
		}
	}
}

// blobTx builds a one-blob type-3 self-transfer with a version-1 (cell
// proof) sidecar, the only sidecar version geth's blobpool accepts once
// Osaka is active. Every 32-byte field element carries the nonce in its low
// bytes; byte 0 stays zero so each scalar remains canonical (< BLS modulus).
func blobTx(key *ecdsa.PrivateKey, signer types.Signer, chainID *big.Int, addr common.Address, nonce uint64) (*types.Transaction, error) {
	var blob kzg4844.Blob
	for i := 0; i < len(blob); i += 32 {
		binary.BigEndian.PutUint64(blob[i+24:i+32], nonce)
	}
	commitment, err := kzg4844.BlobToCommitment(&blob)
	if err != nil {
		return nil, fmt.Errorf("commitment: %w", err)
	}
	cellProofs, err := kzg4844.ComputeCellProofs(&blob) // 128 proofs per blob
	if err != nil {
		return nil, fmt.Errorf("cell proofs: %w", err)
	}
	sidecar := types.NewBlobTxSidecar(types.BlobSidecarVersion1,
		[]kzg4844.Blob{blob}, []kzg4844.Commitment{commitment}, cellProofs)
	return types.MustSignNewTx(key, signer, &types.BlobTx{
		ChainID:    uint256.MustFromBig(chainID),
		Nonce:      nonce,
		To:         addr, // self-transfer; blob txs cannot create contracts
		Value:      uint256.NewInt(1),
		Gas:        21000,
		GasTipCap:  uint256.NewInt(1_000_000_000),
		GasFeeCap:  uint256.NewInt(100_000_000_000),
		BlobFeeCap: uint256.NewInt(10_000_000_000), // blob base fee floors at 1 wei; headroom for bursts
		BlobHashes: sidecar.BlobHashes(),
		Sidecar:    sidecar,
	}), nil
}
