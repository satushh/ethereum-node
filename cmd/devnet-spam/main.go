// Command devnet-spam keeps a devnet's txpool busy; the logic lives in
// internal/devnetspam so `ethereum-node devnet` can run it in-process.
// scripts/devnet-up.sh runs this standalone binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/satushh/ethereum-node/internal/devnetspam"
)

func main() {
	rpcURL := flag.String("rpc", "http://127.0.0.1:8545", "execution JSON-RPC endpoint")
	rate := flag.Int("rate", 30, "total transactions per second across all accounts")
	numAccounts := flag.Int("num-accounts", 3, "number of deterministic dev accounts")
	printAddrs := flag.Bool("print-addrs", false, "print the account addresses and exit (for genesis funding)")
	flag.Parse()

	if *printAddrs {
		for _, addr := range devnetspam.Addresses(*numAccounts) {
			fmt.Println(addr)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := devnetspam.Run(ctx, *rpcURL, *rate, *numAccounts); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "devnet-spam:", err)
		os.Exit(1)
	}
}
