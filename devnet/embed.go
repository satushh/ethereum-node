// Package devnet carries the local devnet fixtures. The chain config is
// embedded so the installed binary can materialize it without a repo
// checkout (`ethereum-node devnet`).
package devnet

import _ "embed"

//go:embed chain-config.yml
var ChainConfigYML []byte
