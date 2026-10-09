package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/OffchainLabs/prysm/v7/runtime/interop"
	"github.com/OffchainLabs/prysm/v7/validator/accounts/wallet"
	"github.com/OffchainLabs/prysm/v7/validator/keymanager"
	"github.com/OffchainLabs/prysm/v7/validator/keymanager/local"
	"github.com/urfave/cli/v2"
)

// Devnet-only wallet password; the wallet holds publicly-known deterministic
// interop keys, so secrecy is irrelevant.
const devnetWalletPassword = "ethereum-node-devnet-password"

// devnetWalletCommand creates a Prysm validator wallet holding the
// deterministic interop keys that `testnet generate-genesis --num-validators`
// premines into the genesis state. This mirrors createValidatorWallet in
// prysm/testing/endtoend/components/validator.go.
func devnetWalletCommand() *cli.Command {
	return &cli.Command{
		Name:  "devnet-wallet",
		Usage: "Create a validator wallet from the deterministic interop keys (devnet only)",
		Flags: []cli.Flag{walletDirFlag, numValidatorsFlag, keyOffsetFlag},
		Action: func(c *cli.Context) error {
			return createDevnetWallet(c.Context,
				c.String(walletDirFlag.Name), c.Uint64(keyOffsetFlag.Name), c.Uint64(numValidatorsFlag.Name))
		},
	}
}

// createDevnetWallet is the devnet-wallet command's engine, also called by
// `ethereum-node devnet` to provision its wallet on first run.
func createDevnetWallet(ctx context.Context, walletDirArg string, offset, numValidators uint64) error {
	walletDir, err := filepath.Abs(walletDirArg)
	if err != nil {
		return err
	}
	// Refuse to clobber arbitrary directories (--wallet-dir=. must
	// not erase a checkout); the caller deletes old wallets.
	entries, err := os.ReadDir(walletDir)
	switch {
	case err == nil && len(entries) > 0:
		return fmt.Errorf("%s already exists and is not empty; remove it first to recreate the wallet", walletDir)
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("inspect wallet dir: %w", err)
	}

	privs, pubs, err := interop.DeterministicallyGenerateKeys(offset, numValidators)
	if err != nil {
		return fmt.Errorf("generate interop keys: %w", err)
	}

	w := wallet.New(&wallet.Config{
		WalletDir:      walletDir,
		KeymanagerKind: keymanager.Local,
		WalletPassword: devnetWalletPassword,
	})
	if err := w.SaveWallet(); err != nil {
		return fmt.Errorf("save wallet: %w", err)
	}
	km, err := local.NewKeymanager(ctx, &local.SetupConfig{Wallet: w})
	if err != nil {
		return fmt.Errorf("create keymanager: %w", err)
	}
	privKeyBytes := make([][]byte, len(privs))
	pubKeyBytes := make([][]byte, len(pubs))
	for i := range privs {
		privKeyBytes[i] = privs[i].Marshal()
		pubKeyBytes[i] = pubs[i].Marshal()
	}
	if err := km.ImportKeypairs(ctx, privKeyBytes, pubKeyBytes); err != nil {
		return fmt.Errorf("import keypairs: %w", err)
	}

	passwordFile := filepath.Join(walletDir, "password.txt")
	if err := os.WriteFile(passwordFile, []byte(devnetWalletPassword), 0o600); err != nil {
		return err
	}
	fmt.Printf("wallet:        %s\n", walletDir)
	fmt.Printf("password file: %s\n", passwordFile)
	fmt.Printf("validators:    %d (interop keys starting at index %d)\n", len(pubs), offset)
	return nil
}
