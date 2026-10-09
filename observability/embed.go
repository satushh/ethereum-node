// Package observability carries the Prometheus + Grafana compose stack. It
// is embedded so the installed binary can materialize and start it without a
// repo checkout (`ethereum-node devnet`); scripts/observability-up.sh keeps
// using the files on disk directly.
package observability

import "embed"

//go:embed docker-compose.yml prometheus.yml grafana
var FS embed.FS
