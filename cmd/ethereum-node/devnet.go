package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	testnetcmds "github.com/OffchainLabs/prysm/v7/cmd/prysmctl/testnet"
	"github.com/urfave/cli/v2"

	"github.com/satushh/ethereum-node/devnet"
	"github.com/satushh/ethereum-node/internal/devnetspam"
	"github.com/satushh/ethereum-node/observability"
)

// The spam accounts and funding mirror scripts/devnet-up.sh: 3 deterministic
// dev accounts at 10^6 ether each, 1 gwei starting base fee.
const (
	devnetSpamAccounts = 3
	devnetSpamBalance  = "0xd3c21bcecceda1000000"
	devnetBaseFee      = "0x3b9aca00"
)

var (
	devnetDatadirFlag = &cli.StringFlag{
		Name:  "datadir",
		Usage: "Devnet root directory; chain state inside is wiped on every start (slots are wall-clock, a stopped devnet cannot resume), the wallet is kept",
		Value: "./ethereum-node-devnet",
	}
	genesisDelayFlag = &cli.Uint64Flag{
		Name:  "genesis-delay",
		Usage: "Seconds from startup to genesis; must cover validator startup",
		Value: 30,
	}
	observabilityFlag = &cli.StringFlag{
		Name:  "observability",
		Usage: "Bundled Prometheus+Grafana via docker compose: auto (when docker is available), on (require docker), off",
		Value: "auto",
	}
	spamRateFlag = &cli.IntFlag{
		Name:  "spam.rate",
		Usage: "Transactions per second from the prefunded dev accounts (0 disables the spammer)",
		Value: 30,
	}
)

func devnetCommand() *cli.Command {
	return &cli.Command{
		Name: "devnet",
		Usage: "Run a self-contained local devnet: the combined node, interop validators, " +
			"a tx spammer, and (with docker) Grafana dashboards. One command, Ctrl-C stops everything",
		Flags: []cli.Flag{
			devnetDatadirFlag, genesisDelayFlag, numValidatorsFlag, spamRateFlag,
			observabilityFlag, engineTransportFlag, verbosityFlag, metricsFlag,
		},
		Action: runDevnet,
	}
}

func runDevnet(c *cli.Context) error {
	// Catch SIGINT/SIGTERM from the start so an interrupt during genesis
	// generation or compose startup still runs the deferred teardown
	// (once the node runs, Prysm's own handler drives shutdown).
	ctx, stopSignals := signal.NotifyContext(c.Context, syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	dd, err := filepath.Abs(c.String(devnetDatadirFlag.Name))
	if err != nil {
		return err
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:8545", 500*time.Millisecond); err == nil {
		conn.Close()
		return fmt.Errorf("127.0.0.1:8545 is already serving; another node is running, stop it first")
	}

	for _, p := range []string{
		filepath.Join(dd, "data"), filepath.Join(dd, "validator"), filepath.Join(dd, "logs"),
		filepath.Join(dd, "genesis.json"), filepath.Join(dd, "genesis.ssz"),
	} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	logsDir := filepath.Join(dd, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return err
	}
	chainCfg := filepath.Join(dd, "chain-config.yml")
	if err := os.WriteFile(chainCfg, devnet.ChainConfigYML, 0o644); err != nil {
		return err
	}

	walletDir := filepath.Join(dd, "wallet")
	numValidators := c.Uint64(numValidatorsFlag.Name)
	if entries, err := os.ReadDir(walletDir); err != nil || len(entries) == 0 {
		fmt.Println("devnet: creating validator wallet (first run)")
		if err := createDevnetWallet(ctx, walletDir, 0, numValidators); err != nil {
			return err
		}
	}

	// Two-pass genesis, like scripts/devnet-up.sh: the spam accounts must be
	// funded BEFORE the consensus genesis state is computed, because the CL
	// state embeds the EL genesis hash.
	delay := c.Uint64(genesisDelayFlag.Name)
	fmt.Printf("devnet: generating genesis (delay %ds, %d validators, %d funded spam accounts)\n",
		delay, numValidators, devnetSpamAccounts)
	template := filepath.Join(dd, "genesis-template.json")
	discard := filepath.Join(dd, "genesis-discard.ssz")
	genesisJSON := filepath.Join(dd, "genesis.json")
	genesisSSZ := filepath.Join(dd, "genesis.ssz")
	if err := runGenesisGen(ctx,
		fmt.Sprintf("--num-validators=%d", numValidators),
		"--chain-config-file="+chainCfg,
		"--geth-genesis-json-out="+template,
		"--output-ssz="+discard,
	); err != nil {
		return fmt.Errorf("genesis pass 1: %w", err)
	}
	if err := fundGenesisAccounts(template, devnetspam.Addresses(devnetSpamAccounts)); err != nil {
		return err
	}
	if err := runGenesisGen(ctx,
		fmt.Sprintf("--num-validators=%d", numValidators),
		fmt.Sprintf("--genesis-time-delay=%d", delay),
		"--chain-config-file="+chainCfg,
		"--geth-genesis-json-in="+template,
		"--geth-genesis-json-out="+genesisJSON,
		"--output-ssz="+genesisSSZ,
	); err != nil {
		return fmt.Errorf("genesis pass 2: %w", err)
	}
	_ = os.Remove(template)
	_ = os.Remove(discard)

	obsDown, err := startObservability(ctx, c.String(observabilityFlag.Name), filepath.Join(dd, "observability"))
	if err != nil {
		return err
	}
	if obsDown != nil {
		defer obsDown()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// The validator and spammer start in the background once the node they
	// talk to is actually up; the node itself blocks this goroutine.
	orchCtx, orchCancel := context.WithCancel(ctx)
	defer orchCancel()
	var childMu sync.Mutex
	var validatorChild *childProc
	validatorLog := filepath.Join(logsDir, "validator.log")
	go func() {
		if !waitHTTP(orchCtx, "http://127.0.0.1:3500/eth/v1/node/version", 90*time.Second) {
			if orchCtx.Err() == nil {
				fmt.Fprintln(os.Stderr, "devnet: ERROR: beacon API not ready after 90s; validator not started")
			}
			return
		}
		cp, err := startValidatorChild(dd, chainCfg, walletDir, validatorLog, c)
		if err != nil {
			fmt.Fprintf(os.Stderr, "devnet: ERROR: validator failed to start: %v\n", err)
			return
		}
		childMu.Lock()
		validatorChild = cp
		childMu.Unlock()
		fmt.Printf("devnet: validator running (pid %d, %d keys, log: %s)\n",
			cp.cmd.Process.Pid, numValidators, validatorLog)

		if rate := c.Int(spamRateFlag.Name); rate > 0 {
			if waitHTTP(orchCtx, "http://127.0.0.1:8545", 60*time.Second) {
				go func() {
					if err := devnetspam.Run(orchCtx, "http://127.0.0.1:8545", rate, devnetSpamAccounts); err != nil && orchCtx.Err() == nil {
						fmt.Fprintf(os.Stderr, "devnet: spammer stopped: %v\n", err)
					}
				}()
			}
		}
		select {
		case <-cp.done:
			if orchCtx.Err() == nil {
				fmt.Fprintf(os.Stderr, "devnet: ERROR: validator exited unexpectedly, no blocks will be proposed (see %s)\n", validatorLog)
			}
		case <-orchCtx.Done():
		}
	}()

	fmt.Printf("devnet: starting node; genesis in %ds, first blocks shortly after. Ctrl-C stops everything.\n", delay)
	runArgs := []string{
		"ethereum-node", "run",
		"--datadir=" + filepath.Join(dd, "data"),
		"--el-genesis=" + genesisJSON,
		"--cl-genesis-state=" + genesisSSZ,
		"--cl-chain-config=" + chainCfg,
		"--engine-transport=" + c.String(engineTransportFlag.Name),
		"--verbosity=" + c.String(verbosityFlag.Name),
		fmt.Sprintf("--metrics=%t", c.Bool(metricsFlag.Name)),
		// configs/devnet.yaml's consensus.settings, as passthrough flags
		"--beacon-flag=no-discovery",
		"--beacon-flag=supernode",
		"--beacon-flag=min-sync-peers=0",
		"--beacon-flag=minimum-peers-per-subnet=0",
		"--beacon-flag=contract-deployment-block=0",
	}
	nodeApp := &cli.App{Name: "ethereum-node", Commands: []*cli.Command{runCommand()}}
	runErr := nodeApp.RunContext(ctx, runArgs)

	orchCancel()
	childMu.Lock()
	cp := validatorChild
	childMu.Unlock()
	if cp != nil {
		cp.stop()
	}
	return runErr
}

// runGenesisGen invokes the embedded prysmctl generator (`ethereum-node
// testnet generate-genesis`) in-process.
func runGenesisGen(ctx context.Context, args ...string) error {
	app := &cli.App{Name: "ethereum-node", Commands: testnetcmds.Commands}
	argv := append([]string{"ethereum-node", "testnet", "generate-genesis", "--fork=fulu"}, args...)
	return app.RunContext(ctx, argv)
}

// fundGenesisAccounts adds the spam accounts to the EL genesis alloc and
// pins the starting base fee, between the two generator passes.
func fundGenesisAccounts(path string, addrs []string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var g map[string]any
	if err := json.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	alloc, _ := g["alloc"].(map[string]any)
	if alloc == nil {
		alloc = map[string]any{}
		g["alloc"] = alloc
	}
	for _, addr := range addrs {
		alloc[addr] = map[string]any{"balance": devnetSpamBalance}
	}
	g["baseFeePerGas"] = devnetBaseFee
	out, err := json.MarshalIndent(g, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// waitHTTP polls url until it answers below HTTP 400, ctx is canceled, or
// maxWait elapses.
func waitHTTP(ctx context.Context, url string, maxWait time.Duration) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				if resp.StatusCode < 400 {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Second):
		}
	}
	return false
}

type childProc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// stop follows the TERM -> wait -> KILL -> verify contract of
// scripts/devnet-up.sh.
func (cp *childProc) stop() {
	if cp.cmd.Process == nil {
		return
	}
	_ = cp.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-cp.done:
		return
	case <-time.After(10 * time.Second):
	}
	fmt.Fprintln(os.Stderr, "devnet: validator ignored SIGTERM; escalating to SIGKILL")
	_ = cp.cmd.Process.Kill()
	select {
	case <-cp.done:
	case <-time.After(3 * time.Second):
		fmt.Fprintln(os.Stderr, "devnet: ERROR: validator survived SIGKILL")
	}
}

// startValidatorChild re-execs this binary's `validator` command as a child
// process: the same topology as a standalone validator client, with no
// in-process fights over prysm's global feature/metrics state.
func startValidatorChild(dd, chainCfg, walletDir, logPath string, c *cli.Context) (*childProc, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	args := []string{
		"validator",
		"--accept-terms-of-use",
		"--datadir=" + filepath.Join(dd, "validator"),
		"--beacon-rpc-provider=127.0.0.1:4000",
		"--chain-config-file=" + chainCfg,
		"--wallet-dir=" + walletDir,
		"--wallet-password-file=" + filepath.Join(walletDir, "password.txt"),
		"--suggested-fee-recipient=" + devnetFeeRecipient,
		"--verbosity=" + c.String(verbosityFlag.Name),
	}
	if c.Bool(metricsFlag.Name) {
		args = append(args, "--monitoring-host=127.0.0.1", "--monitoring-port=8081")
	} else {
		args = append(args, "--disable-monitoring")
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, err
	}
	cp := &childProc{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(cp.done)
	}()
	return cp, nil
}

// startObservability materializes the embedded compose stack and starts it.
// The returned func tears it down (nil when the stack was not started).
// Project name "observability" matches scripts/observability-up.sh, so
// either path manages the same containers.
func startObservability(ctx context.Context, mode, obsDir string) (func(), error) {
	switch mode {
	case "off":
		return nil, nil
	case "auto", "on":
	default:
		return nil, fmt.Errorf("--%s must be auto, on, or off, got %q", observabilityFlag.Name, mode)
	}
	dockerErr := dockerAvailable(ctx)
	if dockerErr != nil {
		if mode == "on" {
			return nil, fmt.Errorf("--%s=on but docker is not usable: %v", observabilityFlag.Name, dockerErr)
		}
		fmt.Printf("devnet: docker not available, skipping Prometheus/Grafana (%v)\n", dockerErr)
		return nil, nil
	}

	if err := materializeFS(observability.FS, obsDir); err != nil {
		return nil, err
	}
	composeFile := filepath.Join(obsDir, "docker-compose.yml")
	up := exec.CommandContext(ctx, "docker", "compose", "-p", "observability", "-f", composeFile, "up", "-d")
	up.Stdout, up.Stderr = os.Stdout, os.Stderr
	if err := up.Run(); err != nil {
		return nil, fmt.Errorf("docker compose up: %w", err)
	}
	fmt.Println("devnet: grafana:    http://127.0.0.1:3001  (no login, dashboard: 'ethereum-node')")
	fmt.Println("devnet: prometheus: http://127.0.0.1:9090")
	return func() {
		downCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		down := exec.CommandContext(downCtx, "docker", "compose", "-p", "observability", "-f", composeFile, "down")
		down.Stdout, down.Stderr = os.Stdout, os.Stderr
		if err := down.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "devnet: docker compose down failed: %v\n", err)
		}
	}, nil
}

func dockerAvailable(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker not in PATH")
	}
	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(infoCtx, "docker", "info").Run(); err != nil {
		return fmt.Errorf("docker daemon not responding")
	}
	return nil
}

func materializeFS(src fs.FS, dst string) error {
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, path)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
