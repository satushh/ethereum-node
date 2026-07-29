package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v3"

	"github.com/satushh/ethereum-node/internal/prysmapp"
)

// fileConfig is the single config file for both halves of the node:
//
//	node:       supervisor-level settings (datadir, network, ...)
//	execution:  the geth options this binary exposes (struct-mapped, so only
//	            the documented keys exist)
//	consensus:  genesis fixtures, raw passthrough flags, and `settings` — a
//	            map of upstream prysm flag names fed verbatim to prysm's own
//	            --config-file loader, so the full beacon flag surface is
//	            reachable from this one file
//
// Explicit CLI flags always win over file values.
type fileConfig struct {
	Node struct {
		Datadir           string `yaml:"datadir"`
		Network           string `yaml:"network"`
		Verbosity         string `yaml:"verbosity"`
		Metrics           *bool  `yaml:"metrics"`
		FeeRecipient      string `yaml:"fee-recipient"`
		CheckpointSyncURL string `yaml:"checkpoint-sync-url"`
	} `yaml:"node"`
	Execution struct {
		HTTPPort    int    `yaml:"http-port"`
		AuthrpcPort int    `yaml:"authrpc-port"`
		P2PListen   string `yaml:"p2p-listen"`
		Genesis     string `yaml:"genesis"`
	} `yaml:"execution"`
	Consensus struct {
		GenesisState string         `yaml:"genesis-state"`
		ChainConfig  string         `yaml:"chain-config"`
		Flags        []string       `yaml:"flags"`
		Settings     map[string]any `yaml:"settings"`
	} `yaml:"consensus"`
}

// applyFileConfig loads the YAML file and applies it onto the cli context,
// leaving any flag the user set explicitly untouched. It returns extra
// passthrough args for the beacon node (consensus.flags plus a generated
// prysm config file for consensus.settings).
func applyFileConfig(c *cli.Context, path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file: %w", err)
	}
	var fc fileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return nil, fmt.Errorf("parse config file %s: %w", path, err)
	}

	set := func(flagName, value string) error {
		if value == "" || c.IsSet(flagName) {
			return nil
		}
		return c.Set(flagName, value)
	}
	for flagName, value := range map[string]string{
		datadirFlag.Name:        fc.Node.Datadir,
		networkFlag.Name:        fc.Node.Network,
		verbosityFlag.Name:      fc.Node.Verbosity,
		feeRecipientFlag.Name:   fc.Node.FeeRecipient,
		checkpointURLFlag.Name:  fc.Node.CheckpointSyncURL,
		p2pListenFlag.Name:      fc.Execution.P2PListen,
		elGenesisFlag.Name:      fc.Execution.Genesis,
		clGenesisStateFlag.Name: fc.Consensus.GenesisState,
		clChainConfigFlag.Name:  fc.Consensus.ChainConfig,
	} {
		if err := set(flagName, value); err != nil {
			return nil, err
		}
	}
	if fc.Execution.HTTPPort != 0 {
		if err := set(httpPortFlag.Name, strconv.Itoa(fc.Execution.HTTPPort)); err != nil {
			return nil, err
		}
	}
	if fc.Execution.AuthrpcPort != 0 {
		if err := set(authPortFlag.Name, strconv.Itoa(fc.Execution.AuthrpcPort)); err != nil {
			return nil, err
		}
	}
	if fc.Node.Metrics != nil {
		if err := set(metricsFlag.Name, strconv.FormatBool(*fc.Node.Metrics)); err != nil {
			return nil, err
		}
	}

	var beaconExtra []string
	for _, f := range fc.Consensus.Flags {
		beaconExtra = append(beaconExtra, "--"+f)
	}
	if len(fc.Consensus.Settings) > 0 {
		// Prysm's config-file loader silently ignores unknown keys, so a
		// typo would become a silent no-op. Validate every key against the
		// embedded beacon node's actual flag set and fail loudly instead.
		known := prysmapp.KnownFlags()
		var unknown []string
		for key := range fc.Consensus.Settings {
			if !known[key] {
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("consensus.settings contains keys the beacon node does not recognize: %v", unknown)
		}
		// Hand the settings map to prysm's own config-file loader: full
		// upstream flag surface, upstream parsing, one operator file.
		out, err := yaml.Marshal(fc.Consensus.Settings)
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp("", "ethereum-node-beacon-*.yaml")
		if err != nil {
			return nil, err
		}
		if _, err := tmp.Write(out); err != nil {
			return nil, err
		}
		if err := tmp.Close(); err != nil {
			return nil, err
		}
		beaconExtra = append(beaconExtra, "--config-file="+tmp.Name())
	}
	return beaconExtra, nil
}
