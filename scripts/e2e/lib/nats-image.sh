# shellcheck shell=bash
# scripts/e2e/lib/nats-image.sh — sourced by every live proof that starts a NATS server of its own
# (nats-image.test.ts finds each by a scan of the tracked scripts). Sets nats_image to
# testnats.HubImage, the constant packages/envoy/internal/testnats/nats.go declares:
# ghcr.io/cybermonklabs/legion/ci, the copy of the hub's nats:2.15.1-alpine (.github/ci-images.yaml).
# The Envoy listener and the Legion daemon refuse a nats-server below the floor
# packages/envoy/natsversion holds, every 2.10 among them, so a proof on any other image would never
# bring its stack up; reading the Go tests' own constant keeps the two from drifting. Needs $root,
# the checkout's root, which every such driver sets. Drivers run under `set -e`, so a failed read
# leaves nats_image empty rather than ending the driver unexplained.
: "${root:?nats-image.sh needs root, the root of the checkout}"
nats_image=$(sed -n 's/^const HubImage = "\(.*\)"$/\1/p' "$root/packages/envoy/internal/testnats/nats.go" 2>/dev/null) || nats_image=""
if [ -z "$nats_image" ]; then
  echo "nats-image.sh: cannot read const HubImage from $root/packages/envoy/internal/testnats/nats.go" >&2
  exit 1
fi
