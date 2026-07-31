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
	"reflect"
	"strings"
	"unicode"

	"github.com/ethereum/go-ethereum/cmd/utils"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/version"
	"github.com/naoina/toml"
)

// Config carries the minimal set of options the combined binary exposes for
// the execution module. Everything else uses upstream defaults.
type Config struct {
	DataDir     string // execution layer data directory
	Network     string // public network preset (hoodi|sepolia|mainnet); "" means custom genesis via GenesisPath
	GenesisPath string // path to a core.Genesis JSON file (devnet genesis; ignored when Network is set)
	HTTPHost    string // eth/net/web3 JSON-RPC host ("" disables HTTP)
	HTTPPort    int
	AuthPort    int    // authenticated engine API port (escape hatch for external CLs; the embedded CL uses IPC)
	P2PListen   string // devp2p listen address
	Verbosity   string // silent|error|warn|info|debug|trace
	MetricsPort int    // prometheus exporter on 127.0.0.1 (/debug/metrics/prometheus); 0 disables

	// SettingsTOML chunks are geth's complete option surface in geth's own
	// config format (the output of `geth dumpconfig`: [Eth], [Node],
	// [Node.P2P], [Metrics] sections). Chunks are decoded in order, later
	// chunks overriding earlier ones (file first, inline flags after), with
	// the same semantics as `geth --config`, including erroring on unknown
	// fields. The fields above — the supervisor-owned topology (datadir,
	// engine socket, ports, network posture, genesis) — always win over
	// this section.
	SettingsTOML []string
}

// gethTomlConfig mirrors cmd/geth's gethConfig struct: the full
// configuration surface of the embedded execution node.
type gethTomlConfig struct {
	Eth     ethconfig.Config
	Node    node.Config
	Metrics metrics.Config
}

// tomlSettings reproduces cmd/geth's decoder behavior: TOML keys are exact
// Go field names, and unknown fields are an error pointing at the godoc of
// the struct they failed to match.
var tomlSettings = toml.Config{
	NormFieldName: func(rt reflect.Type, key string) string { return key },
	FieldToKey:    func(rt reflect.Type, field string) string { return field },
	MissingField: func(rt reflect.Type, field string) error {
		var link string
		if unicode.IsUpper(rune(rt.Name()[0])) && rt.PkgPath() != "main" {
			link = fmt.Sprintf(", see https://godoc.org/%s#%s for available fields", rt.PkgPath(), rt.Name())
		}
		return fmt.Errorf("field '%s' is not defined in %s%s", field, rt.String(), link)
	},
}

// decodeSettings applies the optional TOML settings chunks in order on top
// of upstream defaults, yielding the base configuration the supervisor then
// overrides.
func decodeSettings(chunks []string) (*gethTomlConfig, error) {
	base := &gethTomlConfig{
		Eth:     ethconfig.Defaults,
		Node:    node.DefaultConfig,
		Metrics: metrics.DefaultConfig,
	}
	// Our default HTTP module set (node.DefaultConfig ships only net+web3);
	// seeded before decoding so a TOML [Node] HTTPModules can still override.
	base.Node.HTTPModules = []string{"eth", "net", "web3", "txpool"}
	for _, chunk := range chunks {
		if chunk == "" {
			continue
		}
		if err := tomlSettings.NewDecoder(strings.NewReader(chunk)).Decode(base); err != nil {
			return nil, fmt.Errorf("execution settings: %w", err)
		}
	}
	return base, nil
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

	base, err := decodeSettings(cfg.SettingsTOML)
	if err != nil {
		return nil, err
	}
	genesis, bootnodes, err := networkPreset(cfg)
	if err != nil {
		return nil, err
	}

	// Must run before the eth service is constructed so its meters register
	// against an enabled metrics registry (mirrors cmd/geth ordering).
	metricsCfg := base.Metrics
	if cfg.MetricsPort > 0 {
		metricsCfg.Enabled = true
		metricsCfg.HTTP = "127.0.0.1"
		metricsCfg.Port = cfg.MetricsPort
	}
	if metricsCfg.Enabled {
		utils.SetupMetrics(&metricsCfg)
	}

	nodeCfg := base.Node
	nodeCfg.Name = "geth"
	nodeCfg.Version = fmt.Sprintf("%d.%d.%d-%s", version.Major, version.Minor, version.Patch, version.Meta)
	nodeCfg.DataDir = cfg.DataDir
	nodeCfg.IPCPath = "geth.ipc"
	nodeCfg.HTTPHost = cfg.HTTPHost
	nodeCfg.HTTPPort = cfg.HTTPPort
	nodeCfg.AuthAddr = "127.0.0.1"
	nodeCfg.AuthPort = cfg.AuthPort
	nodeCfg.P2P.ListenAddr = cfg.P2PListen
	if cfg.Network != "" {
		// Public network: find peers via bootnodes + discovery.
		nodeCfg.P2P.BootstrapNodes = bootnodes
		nodeCfg.P2P.NoDiscovery = false
		nodeCfg.P2P.NoDial = false
		nodeCfg.P2P.NAT = nat.Any()
	} else {
		// Devnet: single node, no peers wanted.
		nodeCfg.P2P.NoDiscovery = true
		nodeCfg.P2P.NoDial = true
		nodeCfg.P2P.NAT = nil
	}

	stack, err := node.New(&nodeCfg)
	if err != nil {
		return nil, fmt.Errorf("create geth node stack: %w", err)
	}

	ethCfg := base.Eth
	ethCfg.Genesis = genesis
	ethCfg.NetworkId = genesis.Config.ChainID.Uint64()
	if cfg.Network != "" {
		ethCfg.SyncMode = ethconfig.SnapSync
		utils.SetDNSDiscoveryDefaults(&ethCfg, genesis.ToBlock().Hash())
	} else {
		ethCfg.SyncMode = ethconfig.FullSync
	}

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

// networkPreset resolves the genesis and bootnodes for a public network, or
// loads the custom genesis file for devnets. Public presets come straight
// from geth's own params, so supported networks track upstream.
func networkPreset(cfg Config) (*core.Genesis, []*enode.Node, error) {
	var genesis *core.Genesis
	var urls []string
	switch cfg.Network {
	case "":
		g, err := loadGenesis(cfg.GenesisPath)
		return g, nil, err
	case "hoodi":
		genesis, urls = core.DefaultHoodiGenesisBlock(), params.HoodiBootnodes
	case "sepolia":
		genesis, urls = core.DefaultSepoliaGenesisBlock(), params.SepoliaBootnodes
	case "mainnet":
		genesis, urls = core.DefaultGenesisBlock(), params.MainnetBootnodes
	default:
		return nil, nil, fmt.Errorf("unknown network %q (supported: hoodi, sepolia, mainnet)", cfg.Network)
	}
	bootnodes := make([]*enode.Node, 0, len(urls))
	for _, url := range urls {
		n, err := enode.Parse(enode.ValidSchemes, url)
		if err != nil {
			return nil, nil, fmt.Errorf("parse bootnode %q: %w", url, err)
		}
		bootnodes = append(bootnodes, n)
	}
	return genesis, bootnodes, nil
}

func loadGenesis(path string) (*core.Genesis, error) {
	if path == "" {
		return nil, fmt.Errorf("no execution genesis configured: pass --network=<hoodi|sepolia|mainnet> for a public network or --el-genesis for a devnet")
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
	// Guard before genesis.Config.ChainID is dereferenced downstream.
	if genesis.Config == nil || genesis.Config.ChainID == nil {
		return nil, fmt.Errorf("execution genesis %s has no chain config / chainId", path)
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
