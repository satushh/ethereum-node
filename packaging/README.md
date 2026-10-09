# Packaging: `.deb` + apt repository

Roadmap item 2's packaging leg. A tag push builds the combined binary on
Ubuntu 22.04 runners (amd64 + arm64, old glibc 2.35 baseline for the cgo/blst
binary), wraps each in a `.deb` with nfpm (`packaging/nfpm.yaml`), signs an
apt repository on the `gh-pages` branch, and attaches the debs to a GitHub
Release (`.github/workflows/release.yml`). The channel decision (self-hosted
repo over a Launchpad PPA: serves Debian *and* Ubuntu, ships the exact tested
binary) is recorded on the README's roadmap item 2.

The package installs `/usr/bin/ethereum-node`, a disabled-by-default systemd
unit (`ethereum-node.service`, running as the `ethereum-node` system user
with the datadir in `/var/lib/ethereum-node`), and a conffile
`/etc/ethereum-node/config.yaml` that ships pointing at the Hoodi testnet.

## End-user install

```sh
curl -fsSL https://satushh.github.io/ethereum-node/key.gpg \
  | sudo tee /usr/share/keyrings/ethereum-node.gpg >/dev/null
echo "deb [signed-by=/usr/share/keyrings/ethereum-node.gpg] https://satushh.github.io/ethereum-node stable main" \
  | sudo tee /etc/apt/sources.list.d/ethereum-node.list
sudo apt update && sudo apt install ethereum-node

# review /etc/ethereum-node/config.yaml (network!), then:
sudo systemctl enable --now ethereum-node
```

The installed binary also runs a self-contained local devnet with
dashboards (no checkout, no toolchain; docker optional for Grafana):

```sh
ethereum-node devnet    # Ctrl-C stops node, validators, spam, containers
```

## One-time setup (before the first tag)

Done 2026-10-09 for v0.1.0: signing key
`7CBCC7C4AD04E9DFF4E5E3476FB6566388EDC334` (in the release laptop's gpg
keyring, no passphrase), repo secret `APT_SIGNING_KEY`, Pages serving
`gh-pages` root. Redo these steps only on key rotation.

1. **Signing key** (keep the private key offline; rotate = redo this):

   ```sh
   gpg --quick-generate-key "ethereum-node apt (release signing) <sdas@offchainlabs.com>" ed25519 sign never
   gpg --armor --export-secret-keys <key-id> | gh secret set APT_SIGNING_KEY --repo satushh/ethereum-node
   ```

2. **GitHub Pages**: repo Settings → Pages → Deploy from a branch →
   `gh-pages`, `/ (root)`. The branch appears after the first release run.

3. **Tag**: `git tag v0.1.0 && git push origin v0.1.0`. A `workflow_dispatch`
   run of `release` is the dry run: packages become CI artifacts, nothing
   publishes.

## Local build + install test (what CI does, minus signing)

```sh
# linux binary (native arch) built in a container; module cache mounted
# read-only. nfpm's contents src is not env-expanded, so the payload path is
# fixed: dist/ethereum-node.
docker run --rm -e GOWORK=off -v "$PWD":/src -v "$HOME/go/pkg/mod":/go/pkg/mod:ro \
  -w /src golang:1.26-bookworm go build -trimpath -o dist/ethereum-node ./cmd/ethereum-node

# wrap it in a .deb (nfpm is pure Go; runs fine on macOS)
GOWORK=off VERSION='0.0.0~dev1' ARCH=arm64 \
  go run github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0 package -f packaging/nfpm.yaml -p deb -t dist/

# install it into a clean Debian and prove the payload works
docker run --rm -v "$PWD/dist":/dist debian:bookworm bash -c \
  'apt-get update -qq >/dev/null && apt-get install -y -qq /dist/ethereum-node_*_arm64.deb >/dev/null \
   && ethereum-node version && id ethereum-node \
   && test -f /usr/lib/systemd/system/ethereum-node.service \
   && test -f /etc/ethereum-node/config.yaml && echo DEB_INSTALL_OK'
```

## Known limits (v1)

- The `gh-pages` branch stores the `.deb` pool in git, so it grows with every
  release; fine at this cadence, move the pool to S3/Cloudsmith when it isn't.
- No reproducible-builds story yet (that is the rest of roadmap item 2).
- One suite (`stable`); no per-network or nightly channel.
