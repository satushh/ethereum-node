// Command ethereum-node runs a Geth execution node and a Prysm beacon node in
// one process: one binary, one datadir, one lifecycle.
//
// Geth and Prysm remain independent upstream projects. This binary composes
// their unmodified sources via Go modules (see go.mod replace directives) and
// connects the consensus module to the execution module over the standard
// Engine API, carried on a private IPC socket inside the datadir — no JWT or
// endpoint configuration needed from the operator.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	prysmcmd "github.com/OffchainLabs/prysm/v7/cmd"
	beaconflags "github.com/OffchainLabs/prysm/v7/cmd/beacon-chain/flags"
	beacongenesis "github.com/OffchainLabs/prysm/v7/cmd/beacon-chain/genesis"
	testnetcmds "github.com/OffchainLabs/prysm/v7/cmd/prysmctl/testnet"
	prysmversion "github.com/OffchainLabs/prysm/v7/runtime/version"
	gethversion "github.com/ethereum/go-ethereum/version"
	"github.com/urfave/cli/v2"

	"github.com/satushh/ethereum-node/internal/gethapp"
	"github.com/satushh/ethereum-node/internal/prysmapp"
)

const bundleVersion = "v0.1.0-dev"

// Prefunded miner account from Prysm's interop EL genesis
// (prysm/runtime/interop/genesis.go), used as the default fee recipient.
const devnetFeeRecipient = "0x878705ba3f8bc32fcf7f4caa1a35e72af65cf766"

var (
	datadirFlag = &cli.StringFlag{
		Name:  "datadir",
		Usage: "Root data directory; execution/ and beacon/ subdirectories are created inside",
		Value: "./ethereum-node-data",
	}
	elGenesisFlag = &cli.StringFlag{
		Name:  "el-genesis",
		Usage: "Path to the execution layer genesis.json (generate with `ethereum-node testnet generate-genesis`)",
	}
	clGenesisStateFlag = &cli.StringFlag{
		Name:  "cl-genesis-state",
		Usage: "Path to the consensus layer genesis state SSZ (generate with `ethereum-node testnet generate-genesis`)",
	}
	clChainConfigFlag = &cli.StringFlag{
		Name:  "cl-chain-config",
		Usage: "Path to the consensus layer chain config YAML",
	}
	httpPortFlag = &cli.IntFlag{
		Name:  "http.port",
		Usage: "Execution JSON-RPC port on 127.0.0.1",
		Value: 8545,
	}
	authPortFlag = &cli.IntFlag{
		Name:  "authrpc.port",
		Usage: "Authenticated engine API port on 127.0.0.1 (escape hatch for external consensus clients; the embedded one uses IPC)",
		Value: 8551,
	}
	p2pListenFlag = &cli.StringFlag{
		Name:  "p2p.listen",
		Usage: "devp2p listen address for the execution module",
		Value: "127.0.0.1:30303",
	}
	feeRecipientFlag = &cli.StringFlag{
		Name:  "fee-recipient",
		Usage: "Suggested fee recipient passed to the beacon node",
		Value: devnetFeeRecipient,
	}
	verbosityFlag = &cli.StringFlag{
		Name:  "verbosity",
		Usage: "Log level for both modules (error|warn|info|debug|trace)",
		Value: "info",
	}
	beaconFlagPassthrough = &cli.StringSliceFlag{
		Name:  "beacon-flag",
		Usage: "Extra flag for the embedded beacon node, without leading dashes (repeatable), e.g. --beacon-flag supernode",
	}
	walletDirFlag = &cli.StringFlag{
		Name:  "wallet-dir",
		Usage: "Directory for the generated validator wallet",
		Value: "./devnet-wallet",
	}
	numValidatorsFlag = &cli.Uint64Flag{
		Name:  "num-validators",
		Usage: "Number of deterministic interop validator keys to import",
		Value: 64,
	}
	keyOffsetFlag = &cli.Uint64Flag{
		Name:  "offset",
		Usage: "Starting index of the deterministic interop keys",
		Value: 0,
	}
)

func main() {
	app := &cli.App{
		Name:    "ethereum-node",
		Usage:   "Geth (execution) + Prysm (consensus) in one binary and one process",
		Version: bundleVersion,
		Commands: append([]*cli.Command{
			runCommand(),
			beaconCommand(),
			devnetWalletCommand(),
			versionCommand(),
		}, testnetcmds.Commands...),
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCommand() *cli.Command {
	return &cli.Command{
		Name:  "run",
		Usage: "Run the combined execution + consensus node",
		Flags: []cli.Flag{
			datadirFlag, elGenesisFlag, clGenesisStateFlag, clChainConfigFlag,
			httpPortFlag, authPortFlag, p2pListenFlag, feeRecipientFlag,
			verbosityFlag, beaconFlagPassthrough,
		},
		Action: runNode,
	}
}

func runNode(c *cli.Context) error {
	datadir, err := filepath.Abs(c.String(datadirFlag.Name))
	if err != nil {
		return err
	}

	fmt.Printf("ethereum-node %s starting\n", bundleVersion)
	fmt.Printf("  execution: geth v%d.%d.%d-%s -> %s\n", gethversion.Major, gethversion.Minor, gethversion.Patch, gethversion.Meta, filepath.Join(datadir, "execution"))
	fmt.Printf("  consensus: prysm %s -> %s\n", prysmversion.Version(), filepath.Join(datadir, "beacon"))

	gethNode, err := gethapp.Start(gethapp.Config{
		DataDir:     filepath.Join(datadir, "execution"),
		GenesisPath: c.String(elGenesisFlag.Name),
		HTTPHost:    "127.0.0.1",
		HTTPPort:    c.Int(httpPortFlag.Name),
		AuthPort:    c.Int(authPortFlag.Name),
		P2PListen:   c.String(p2pListenFlag.Name),
		Verbosity:   c.String(verbosityFlag.Name),
	})
	if err != nil {
		return fmt.Errorf("execution module failed to start: %w", err)
	}
	defer func() {
		fmt.Println("ethereum-node: stopping execution module")
		if err := gethNode.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "ethereum-node: execution module shutdown error: %v\n", err)
		}
	}()
	fmt.Printf("  engine API: %s (in-datadir IPC, no JWT needed)\n", gethNode.IPCEndpoint())

	beaconArgs := []string{
		"beacon-chain",
		"--" + prysmcmd.AcceptTosFlag.Name,
		fmt.Sprintf("--%s=%s", prysmcmd.DataDirFlag.Name, filepath.Join(datadir, "beacon")),
		fmt.Sprintf("--%s=%s", beaconflags.ExecutionEngineEndpoint.Name, gethNode.IPCEndpoint()),
		fmt.Sprintf("--%s=%s", beaconflags.SuggestedFeeRecipient.Name, c.String(feeRecipientFlag.Name)),
		fmt.Sprintf("--%s=%s", prysmcmd.VerbosityFlag.Name, c.String(verbosityFlag.Name)),
		"--" + prysmcmd.DisableMonitoringFlag.Name,
	}
	if v := c.String(clChainConfigFlag.Name); v != "" {
		beaconArgs = append(beaconArgs, fmt.Sprintf("--%s=%s", prysmcmd.ChainConfigFileFlag.Name, v))
	}
	if v := c.String(clGenesisStateFlag.Name); v != "" {
		beaconArgs = append(beaconArgs, fmt.Sprintf("--%s=%s", beacongenesis.StatePath.Name, v))
	}
	for _, f := range c.StringSlice(beaconFlagPassthrough.Name) {
		beaconArgs = append(beaconArgs, "--"+f)
	}

	// Blocks until shutdown; Prysm handles SIGINT/SIGTERM itself, then the
	// deferred Close above tears down the execution module.
	return prysmapp.Run(c.Context, beaconArgs)
}

func beaconCommand() *cli.Command {
	return &cli.Command{
		Name:            "beacon",
		Usage:           "Invoke the embedded Prysm beacon-chain CLI directly (full upstream flag surface)",
		SkipFlagParsing: true,
		Action: func(c *cli.Context) error {
			return prysmapp.Run(c.Context, append([]string{"beacon-chain"}, c.Args().Slice()...))
		},
	}
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "Print the versions of the bundled components",
		Action: func(*cli.Context) error {
			fmt.Printf("ethereum-node %s\n", bundleVersion)
			fmt.Printf("  geth:    v%d.%d.%d-%s\n", gethversion.Major, gethversion.Minor, gethversion.Patch, gethversion.Meta)
			fmt.Printf("  prysm:   %s\n", prysmversion.Version())
			fmt.Printf("  runtime: single process, engine API over private IPC\n")
			return nil
		},
	}
}
