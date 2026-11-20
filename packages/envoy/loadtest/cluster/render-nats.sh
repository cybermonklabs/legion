#!/usr/bin/env bash
# render-nats.sh: regenerates manifests/nats.yaml, the cluster copy's three-pod NATS JetStream hub,
# so that a run needs no helm and no chart download.
#
#   render-nats.sh [--out FILE] [--pin-from FILE]
#
# It renders chart nats/nats 2.15.1 from nats-2.15.1.tgz, the chart's archive vendored beside it,
# and refuses that archive, naming it, before helm reads it when its sha256 is not b209c627…58cc1,
# the digest the chart index at https://nats-io.github.io/k8s/helm/charts/ and the GitHub release
# both publish (the value INFRA-549 records). It renders it with `helm template --skip-tests` (helm
# v3.20.2, which go install fetches and builds under .github/scripts/network-install.sh's bound for
# go) and nats-values.yaml, with each image pinned to the digest a registry manifest read gives its
# tag, and refuses a render that holds a kind the load-test Role cannot write or an image not
# pinned by digest. The output keeps the placeholders __NAMESPACE__, __STORAGE_CLASS__ and
# __RUN_ID__, which cluster.sh fills.
#
# --out FILE writes FILE instead of manifests/nats.yaml. --pin-from FILE takes each image's digest
# from FILE (an earlier render) instead of the registry: cluster.test.sh re-renders that way and
# compares, so a tag that moved since the committed render does not fail the comparison.
set -euo pipefail

here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=packages/envoy/loadtest/cluster/cluster.sh
source "$here/cluster.sh"

# The vendored archive, nats-$CHART_VERSION.tgz, is the release asset
#   https://github.com/nats-io/k8s/releases/download/nats-$CHART_VERSION/nats-$CHART_VERSION.tgz
# and CHART_SHA256 is the digest the chart index ($CHART_REPO/index.yaml) and that release both
# publish for it.
# To bump to version V: download nats-V.tgz beside this script from that URL with V for
# $CHART_VERSION, and git rm the old archive; set CHART_VERSION to V; set CHART_SHA256 to the sum
# this command writes, once it equals the `digest:` the index lists for V:
#   sha256sum nats-V.tgz
# Set each tag in IMAGES below to the default the new chart's values.yaml gives that image. Then
# change the version and sum this file's header names, and the chart version nats-values.yaml and
# ../README.md name; run render-nats.sh; and commit the archive, those files and
# manifests/nats.yaml together.
CHART_REPO=https://nats-io.github.io/k8s/helm/charts
CHART_VERSION=2.15.1
CHART_SHA256=b209c62784b1b24c714d5ff6503b4c7dd948ec187d4222beee16b579f1258cc1
CHART=$here/nats-$CHART_VERSION.tgz
HELM=helm.sh/helm/v3/cmd/helm@v3.20.2
# Each image: the chart value that names it, and the tag render-nats.sh pins (the chart's own
# defaults, on mirror.gcr.io).
IMAGES=(
  "container.image mirror.gcr.io/library/nats:2.15.1-alpine"
  "reloader.image mirror.gcr.io/natsio/nats-server-config-reloader:0.24.0"
  "natsBox.container.image mirror.gcr.io/natsio/nats-box:0.20.0"
)
# The kinds the load-test Role may write that the chart renders; it may write no
# PodDisruptionBudget and no ServiceAccount.
ALLOWED_KINDS=" ConfigMap Deployment Secret Service StatefulSet "

out=$here/manifests/nats.yaml
pin_from=""
while [ $# -gt 0 ]; do
  [ $# -ge 2 ] || die "$1 needs a value"
  case $1 in
    --out) out=$2 ;;
    --pin-from) pin_from=$2 ;;
    *) usage ;;
  esac
  shift 2
done
[ -z "$pin_from" ] || [ -f "$pin_from" ] || die "--pin-from $pin_from is not a file"
for tool in curl go sha256sum jq; do
  command -v "$tool" >/dev/null || die "render-nats.sh needs $tool on PATH"
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/render-nats.XXXXXX")
trap 'rm -rf -- "$tmp"' EXIT

# helm reads the copy whose sha256 was checked, so no change to the vendored file after the check
# reaches the render.
[ -f "$CHART" ] || die "$CHART, the vendored chart archive, is missing"
cp -- "$CHART" "$tmp/nats.tgz"
sha=$(sha256_of "$tmp/nats.tgz")
[ "$sha" = "$CHART_SHA256" ] || die "$CHART has sha256 $sha, not $CHART_SHA256; refusing it"

sets=()
pins=()
for entry in "${IMAGES[@]}"; do
  read -r key ref <<<"$entry"
  if [ -n "$pin_from" ]; then
    repo=${ref%:*}
    pinned=$(grep -oE "image: ${repo//./\\.}@sha256:[0-9a-f]{64}" "$pin_from" | head -n1 || true)
    [ -n "$pinned" ] || die "--pin-from $pin_from pins no $repo"
    pinned=${pinned#image: }
  else
    pinned=$(resolve_image "$ref") || die "$ref does not resolve to a digest"
  fi
  sets+=(--set "$key.fullImageName=$pinned")
  pins+=("$ref -> $pinned")
done

# helm comes from its module at the pinned version, outside any module of this repository as `go
# run` of it did. go-fetch.sh fetches it through the module proxy under network-install.sh's bound
# for go, so a proxy that stalls fails this script by name; the build then reads only the module
# cache (GOPROXY a file:// proxy of it), so the bound never times its compile, and the render runs
# the binary that built.
"$here/../../../../.github/scripts/go-fetch.sh" "$HELM"
(cd -- "$tmp" && GOBIN="$tmp/bin" GOPROXY="file://$(go env GOMODCACHE)/cache/download" go install "$HELM")
(cd -- "$tmp" && "$tmp/bin/helm" template nats nats.tgz --skip-tests -f "$here/nats-values.yaml" "${sets[@]}") >"$tmp/rendered.yaml" ||
  die "helm template failed"

while read -r kind; do
  [[ $ALLOWED_KINDS == *" $kind "* ]] || die "the render holds a $kind, which the load-test Role cannot write; turn it off in nats-values.yaml"
done < <(sed -n 's/^kind: //p' "$tmp/rendered.yaml" | sort -u)
if grep -E '^[[:space:]]*image:' "$tmp/rendered.yaml" | grep -vqE '@sha256:[0-9a-f]{64}$'; then
  die "the render holds an image not pinned by digest: $(grep -E '^[[:space:]]*image:' "$tmp/rendered.yaml" | grep -vE '@sha256:' | tr -s ' ' | tr '\n' ' ')"
fi

{
  printf '# nats.yaml: rendered by render-nats.sh (do not edit): chart nats/nats %s from\n' "$CHART_VERSION"
  printf '# %s/\n# (archive sha256 %s),\n' "$CHART_REPO" "$CHART_SHA256"
  printf '# helm template --skip-tests at helm v3.20.2 with nats-values.yaml, each image pinned to\n'
  printf '# its digest:\n'
  printf '#   %s\n' "${pins[@]}"
  printf '# cluster.sh fills the namespace, storage class and run id placeholders.\n'
  cat -- "$tmp/rendered.yaml"
} >"$out.tmp"
mv -- "$out.tmp" "$out"
printf 'render-nats.sh: wrote %s\n' "$out" >&2
