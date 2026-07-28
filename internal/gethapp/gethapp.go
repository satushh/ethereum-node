// Package gethapp embeds go-ethereum as an in-process module: the execution
// half of the combined ethereum-node binary. It is a stripped-down equivalent
// of cmd/geth's makeFullNode(), wired from Go config structs instead of CLI
// flags, so the go-ethereum clone needs no patches.
package gethapp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/version"
)

// Config carries the minimal set of options the combined binary exposes for
// the execution module. Everything else uses upstream defaults.
type Config struct {
	DataDir     string // execution layer data directory
	GenesisPath string // path to a core.Genesis JSON file (devnet genesis)
	HTTPHost    string // eth/net/web3 JSON-RPC host ("" disables HTTP)
	HTTPPort    int
	AuthPort    int    // authenticated engine API port (escape hatch for external CLs; the embedded CL uses IPC)
	P2PListen   string // devp2p listen address
	Verbosity   string // silent|error|warn|info|debug|trace
}

// Node is a running embedded geth instance.
type Node struct {
	stack *node.Node
}

// Start builds and starts the geth node stack: chain database, networking,
// JSON-RPC (HTTP + IPC) and the engine API (catalyst). The engine API is
// reachable on the returned node's IPC endpoint, which is how the in-process
// Prysm module connects.
func Start(cfg Config) (*Node, error) {
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, verbosityLevel(cfg.Verbosity), false)))

	genesis, err := loadGenesis(cfg.GenesisPath)
	if err != nil {
		return nil, err
	}

	nodeCfg := node.DefaultConfig
	nodeCfg.Name = "geth"
	nodeCfg.Version = fmt.Sprintf("%d.%d.%d-%s", version.Major, version.Minor, version.Patch, version.Meta)
	nodeCfg.DataDir = cfg.DataDir
	nodeCfg.IPCPath = "geth.ipc"
	nodeCfg.HTTPHost = cfg.HTTPHost
	nodeCfg.HTTPPort = cfg.HTTPPort
	nodeCfg.HTTPModules = []string{"eth", "net", "web3", "txpool"}
	nodeCfg.HTTPVirtualHosts = []string{"localhost"}
	nodeCfg.AuthAddr = "127.0.0.1"
	nodeCfg.AuthPort = cfg.AuthPort
	nodeCfg.P2P.ListenAddr = cfg.P2PListen
	nodeCfg.P2P.NoDiscovery = true
	nodeCfg.P2P.NoDial = true
	nodeCfg.P2P.NAT = nil

	stack, err := node.New(&nodeCfg)
	if err != nil {
		return nil, fmt.Errorf("create geth node stack: %w", err)
	}

	ethCfg := ethconfig.Defaults
	ethCfg.Genesis = genesis
	ethCfg.NetworkId = genesis.Config.ChainID.Uint64()
	ethCfg.SyncMode = ethconfig.FullSync

	backend, ethService := utils.RegisterEthService(stack, &ethCfg)
	utils.RegisterFilterAPI(stack, backend, &ethCfg)
	if err := catalyst.Register(stack, ethService); err != nil {
		stack.Close()
		return nil, fmt.Errorf("register engine API: %w", err)
	}
	if err := stack.Start(); err != nil {
		stack.Close()
		return nil, fmt.Errorf("start geth stack: %w", err)
	}
	return &Node{stack: stack}, nil
}

// IPCEndpoint returns the path of the IPC socket serving all APIs including
// the engine API.
func (n *Node) IPCEndpoint() string {
	return n.stack.IPCEndpoint()
}

// HTTPEndpoint returns the user JSON-RPC endpoint, or "" if HTTP is disabled.
func (n *Node) HTTPEndpoint() string {
	return n.stack.HTTPEndpoint()
}

// Close performs a graceful shutdown of the geth stack.
func (n *Node) Close() error {
	return n.stack.Close()
}

func loadGenesis(path string) (*core.Genesis, error) {
	if path == "" {
		return nil, fmt.Errorf("no execution genesis file configured (public network presets are not wired up yet; pass --el-genesis)")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open execution genesis: %w", err)
	}
	defer f.Close()
	genesis := new(core.Genesis)
	if err := json.NewDecoder(f).Decode(genesis); err != nil {
		return nil, fmt.Errorf("decode execution genesis %s: %w", path, err)
	}
	return genesis, nil
}

func verbosityLevel(v string) slog.Level {
	switch v {
	case "silent":
		return log.LevelCrit
	case "error":
		return log.LevelError
	case "warn":
		return log.LevelWarn
	case "debug":
		return log.LevelDebug
	case "trace":
		return log.LevelTrace
	default:
		return log.LevelInfo
	}
}
