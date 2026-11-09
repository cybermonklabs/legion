#!/usr/bin/env bash
# Fills the copy of every `second_source: ghcr-mirror` row of .github/ci-tools.yaml in the
# package that file's `registry:` names.
# An hour of 504s on GitHub's release assets holds every merge of this repository, because each
# of those installs reads one source and has nowhere else to go (dispatch://INFRA-1873).
# The copy in the package is the second source those installs read, and this script is what puts
# the bytes in it.
#
# THE BLOB'S ADDRESS IS THE PINNED DIGEST, SO THIS SCRIPT PUSHES WITH curl AND NOT `crane append`.
# `crane append` re-compresses the layer it writes, and the blob's digest would then stop being
# the asset's digest.
# That digest is the whole point: the blob's address in the package is the one literal both paths
# check.
# .github/scripts/ci-tools.sh reads the copy at /v2/<path>/blobs/sha256:<the row's digest> and
# holds whatever it gets to that same string, so a blob stored under any other digest is a blob
# no install can find and no install would accept.
# This script therefore pushes the asset's own bytes through the registry API, which stores a
# blob under the digest of the bytes it is given, and builds the small config and manifest around
# them itself.
#
# THIS SCRIPT IS THE ONLY JUDGE OF THAT COPY'S STATE.
# It reads the package, it decides what each row needs, and it says what it did.
# No second step of the same job can refuse a row this one allowed.
# .github/scripts/check-ci-tools.sh, the gate beside it, reads the tree alone and has no mode
# that reads the package, for that reason: a cold row is green here, and a second reader with a
# rule of its own would redden the run during the outage the copy exists to survive.
#
# THE TOKEN.
# The package is public to read and needs a login to write, so this run asks
# .github/scripts/ci-tools.sh for a `pull,push` token with GITHUB_ACTOR:GITHUB_TOKEN, through
# the one token request this repository makes (`ci_tools_ghcr_token`), which an install makes
# with that helper's own defaults for an anonymous `pull`.
# ci.yaml's ci-images job holds `packages: write` and sets both variables, and a run with either
# of them empty is refused before it reads anything.
#
# WHAT THIS SCRIPT DECIDES, AND WHAT IT READS FROM THE HELPER BESIDE IT.
# The copy is written here and read by every install through .github/scripts/ci-tools.sh, so the
# two must agree on the row, the digest and the address, and this script holds no second copy of
# any of that: it reads the rows with `ci_tools_rows`, each row's second source with
# `ci_tools_source_name` and their fields with `ci_tools_field`, fetches the asset with
# `ci_tools_curl` and asks the package every question of its own with `ci_tools_curl_status`,
# so every request this run makes rides the one retry ladder and none of them is written here
# a second time; it holds the bytes to the row with `ci_tools_verify`, names the package with
# `ci_tools_registry`, addresses that package with `ci_tools_registry_url` and the row's blob in
# it with `ci_tools_blob_url`, and mints its token with `ci_tools_ghcr_token`.
# What is this script's own is what no install does: the state of the copy — `held`, `filled` or
# `cold` — the blob and manifest pushes, the read-back, and the refusal of a blob the package
# held and no longer holds.
#
# WHAT IT DOES WITH EACH ghcr-mirror ROW, IN ORDER.
#
#   1. It asks the package for the row's blob, and for the tag that names it.
#      A blob the package holds under a tag that resolves is `held`, and nothing is pushed.
#      A blob it holds under a tag that does not resolve gets the manifest pushed, because an
#      untagged blob is a package version nothing names and a retention policy may take it.
#      A blob the package lacks under a tag that resolves is refused, naming the row: the package
#      held this blob and no longer holds it, which is the garbage collection that tag exists to
#      stop, and a run that quietly filled it again would hide that.
#      A blob it lacks under a tag that does not resolve is filled.
#      Any other status from either read is refused, naming the row, the URL and the status.
#   2. It fetches the row's `mirror_from` with the row's own `max_time`.
#      An asset that does not answer leaves the row `cold`: this run prints one warning beside it,
#      and exits 0 for that row.
#      That is the one case that stays green, because the asset is down, the blob is absent, and
#      nothing can be done.
#      Each install keeps the single source it has today until a later run fills the copy.
#      Where the row names a `mirror_member`, that member is taken out of the archive, and those
#      are the bytes.
#   3. It holds those bytes to the row's `digest` with `ci_tools_verify`, the check every
#      install of the copy runs on what it fetched.
#      A mismatch is refused there, naming the row and both digests, and here, naming the URL
#      that served the bytes, and nothing is pushed.
#   4. It pushes the layer blob, then the config blob, then the manifest at the row's tag, each
#      with the status the registry API gives it.
#      Any other status is refused, naming the row, the request and the status.
#   5. It reads the blob back.
#      Anything but 200 is refused, naming the row: an asset that answered and a fill that then
#      did not land is a defect, not an outage.
#
# An `npm` row needs no copy of ours, and this run prints one line per such row and reads
# nothing for it.
#
# It prints exactly one of `held`, `filled` or `cold` per ghcr-mirror row, one line per npm row,
# and one summary line at the end that counts each.
#
# THE THREE SEAMS, each read with a default, so a CI job sets none of them. They are
# .github/scripts/ci-tools.sh's own names, so one reader learns one set:
#
#   CI_TOOLS_MANIFEST     the manifest. Default .github/ci-tools.yaml, relative to the repository
#                         root this script cd's to.
#   CI_TOOLS_GHCR_BASE    default https://ghcr.io, applied by that helper's own
#                         ci_tools_registry_url, which every URL this run reads or writes is
#                         built from.
#   CI_TOOLS_GITHUB_BASE  default https://github.com, applied to a row's `mirror_from` by that
#                         helper's own ci_tools_github_url, which is what applies it to a row's
#                         `primary`.
#
# .github/scripts/mirror-ci-tools.test.sh serves the package and the release host from a local
# HTTP origin through those seams, so no case of it reaches the network.
#
# Run from anywhere: .github/scripts/mirror-ci-tools.sh
# CI runs it in ci.yaml's ci-images job, after that job's image copy, under the login that job
# already holds for the package. Its tests run in ci.yaml's test job.
set -euo pipefail

# The directory of this script, resolved before the cd below, so the helper beside it is found
# however this script was named on the command line.
script_dir=$(cd "$(dirname "$0")" && pwd)
cd "$script_dir/../.."

# The fetch helper every install goes through, which is where this script reads the manifest,
# the digest check, the token, the fetch and the blob's address from, so the filler and the
# reader of a copy cannot disagree about any of them. It defines only ci_tools_* names and runs
# nothing at load.
# shellcheck source=.github/scripts/ci-tools.sh
. "$script_dir/ci-tools.sh"

# The one manifest this run reads: the annotations below name this path, and the helper reads
# the same file through this same seam.
manifest=${CI_TOOLS_MANIFEST:-.github/ci-tools.yaml}
CI_TOOLS_MANIFEST=$manifest

# The media types of the copy. A row that pushes an archive whole pushes a gzipped tar layer, and
# a row that pushes a member taken out of one pushes those bytes as they are.
ARCHIVE_LAYER=application/vnd.oci.image.layer.v1.tar+gzip
MEMBER_LAYER=application/octet-stream
CONFIG_TYPE=application/vnd.oci.image.config.v1+json
MANIFEST_TYPE=application/vnd.oci.image.manifest.v1+json

error() { printf '::error file=%s::%s\n' "$manifest" "$*" >&2; }

if [ -z "${GITHUB_ACTOR:-}" ] || [ -z "${GITHUB_TOKEN:-}" ]; then
  error "this run has no GITHUB_ACTOR and GITHUB_TOKEN to mint a push token from, and it writes to the package ci.yaml's ci-images job logs in to, so it reads nothing and fills nothing."
  exit 1
fi

work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/mirror-ci-tools.XXXXXX")
trap 'rm -rf "$work"' EXIT
headers="$work/headers"

# Every row of the manifest, in its own order. There is one list: each row's second source is
# read in the loop below, so a read of it that fails stops this run where it failed, rather
# than leaving every row to be reported as one that needs no copy.
rows=$(ci_tools_rows)
if [ -z "$rows" ]; then
  error "this file holds no row, so there is nothing to fill."
  exit 1
fi

# The package every copy lives in, which the manifest names once for the whole file, and the
# registry API address of it. Both come from the helper, so the address this run pushes to is
# the address every install reads the copy at.
registry=$(ci_tools_registry)
if [ -z "$registry" ]; then
  error "this file names no registry:, and every copy this run fills lives in that package."
  exit 1
fi
registry_url=$(ci_tools_registry_url)
# The scheme and host of that address, which an upload location written as a path is resolved
# against: a location of that shape is absolute from the root of the host the push went to, and
# it already carries whatever path that host serves the registry API under.
# So the address's own path comes off here rather than staying in front of the location.
# ghcr.io serves the API at its root, where the two readings are the same string; they differ
# wherever CI_TOOLS_GHCR_BASE puts the package behind a path, which is the case
# `an upload location written as a path, with the package behind a path of its own` of
# .github/scripts/mirror-ci-tools.test.sh.
origin_scheme=${registry_url%%://*}
origin_host=${registry_url#*://}
origin_host=${origin_host%%/*}
origin="$origin_scheme://$origin_host"

# The push token: the helper's one token request, asked for `pull,push` with the credential
# ci.yaml's ci-images job holds, where an install asks the same request for an anonymous
# `pull`. The empty first argument takes the helper's own per-attempt bound.
# Every request below carries this token, because the same run writes to the package it reads.
if ! token=$(ci_tools_ghcr_token "" pull,push "$GITHUB_ACTOR:$GITHUB_TOKEN"); then
  error "the registry would not answer a pull,push token request for $registry ($token), so this run reads nothing and fills nothing."
  exit 1
fi

# registry_head <url> [curl argument…]: the HTTP status of one HEAD of the package, with that
# answer's headers left in $headers. A registry may answer a read of a blob with a redirect to
# the store that holds it, so curl follows it and this reads the status of the last hop. A
# request the service did not answer at all reads 000, which is no status this script admits, so
# it is refused like any other.
# The request is made with `ci_tools_curl_status`, the one ladder .github/scripts/ci-tools.sh
# holds, so a flap of the package costs a retry here as it costs one in an install, and this
# file writes no second copy of that ladder (dispatch://INFRA-1545, R2). That helper reads the
# status rather than failing on it, which is what this function is for: the 404 that says the
# package holds no such blob is this run's answer and not its error.
# 30 s per attempt: a HEAD carries no body in either direction, so what an attempt waits for is
# the package's own answer, and 30 s is the bound this repository gives such a request (the
# ci-ok report's `gh api` reads). A row's own `max_time` bounds the transfers of its bytes
# below, which is what that field is about.
registry_head() {
  local url=$1 status
  shift
  status=$(ci_tools_curl_status 30 "$url" -D "$headers" -I \
    -H "Authorization: Bearer $token" "$@") || status=000
  printf '%s\n' "$status"
}

# header_value <name>: that header of the answer $headers holds, empty where it holds none.
header_value() {
  awk -v want="$1:" '
    BEGIN { want = tolower(want) }
    tolower($1) == want { value = $2 }
    END { gsub(/\r/, "", value); print value }
  ' "$headers"
}

# push_blob <row> <file> <digest> <what these bytes are> <per-attempt bound>: one blob of the
# copy, in the two steps the registry API takes. The upload's location may be absolute or
# path-only, and it may already carry a query, so the digest is appended with the separator
# that location needs.
# Both requests go through `ci_tools_curl_status` as the reads above do, so every request this
# run makes retries under the one ladder and the push of a copy rides out the flap an install
# of it would ride out.
# The POST that opens the upload carries no body, so it is bounded at 30 s like the HEADs; the
# PUT that carries the bytes is bounded by the caller, which passes the row's own `max_time`
# for the layer — the bound .github/scripts/ci-tools.sh reads those same bytes under — and
# 30 s for the config document this run wrote itself.
push_blob() {
  local row=$1 file=$2 digest=$3 what=$4 max_time=$5 status location separator
  status=$(ci_tools_curl_status 30 "$registry_url/blobs/uploads/" -X POST -D "$headers" \
    -H "Authorization: Bearer $token" -H 'Content-Length: 0') || status=000
  if [ "$status" != 202 ]; then
    error "$row: the upload of the $what started with POST $registry_url/blobs/uploads/, and the package answered $status where the registry API answers 202. The copy of this row is unchanged."
    return 1
  fi
  location=$(header_value location)
  case "$location" in
    http://* | https://*) ;;
    /*) location="$origin$location" ;;
    *)
      error "$row: the upload of the $what answered 202 with Location '$location', and this run reads an absolute URL or a path. The copy of this row is unchanged."
      return 1
      ;;
  esac
  case "$location" in
    *\?*) separator='&' ;;
    *) separator='?' ;;
  esac
  status=$(ci_tools_curl_status "$max_time" "${location}${separator}digest=sha256:$digest" \
    -X PUT -H "Authorization: Bearer $token" -H 'Content-Type: application/octet-stream' \
    --data-binary "@$file") || status=000
  if [ "$status" != 201 ]; then
    error "$row: the $what was PUT to $location and the package answered $status where the registry API answers 201. The copy of this row is unchanged."
    return 1
  fi
}

# push_copy <row> <layer digest> <layer size> <layer media type> <title> <tag>: the config blob
# and the manifest, which are the same two pushes whether this run filled the blob now or found
# it already there under no tag. Both documents are built with python3, so no shell quoting
# decides a byte of either.
# Both are small documents this run wrote itself — the config is three keys and the manifest
# four — so each is pushed at 30 s per attempt, the bound the requests that carry no asset
# bytes take here, through the same `ci_tools_curl_status` ladder as every other request.
push_copy() {
  local row=$1 digest=$2 size=$3 media=$4 title=$5 tag=$6
  local config="$work/config.json" document="$work/manifest.json" config_digest config_size status
  CI_TOOLS_LAYER="sha256:$digest" PYTHONDONTWRITEBYTECODE=1 python3 - "$config" <<'PY'
import json
import os
import sys

document = {
    "architecture": "amd64",
    "os": "linux",
    "rootfs": {"type": "layers", "diff_ids": [os.environ["CI_TOOLS_LAYER"]]},
}
with open(sys.argv[1], "w") as handle:
    handle.write(json.dumps(document, separators=(",", ":")))
PY
  config_digest=$(sha256sum -- "$config")
  config_digest=${config_digest%% *}
  config_size=$(wc -c < "$config")
  push_blob "$row" "$config" "$config_digest" "config of the copy" 30 || return 1
  PYTHONDONTWRITEBYTECODE=1 python3 - "$document" "$MANIFEST_TYPE" "$CONFIG_TYPE" \
    "$config_digest" "$config_size" "$media" "$digest" "$size" "$title" <<'PY'
import json
import sys

(
    path,
    manifest_type,
    config_type,
    config_digest,
    config_size,
    media,
    layer_digest,
    layer_size,
    title,
) = sys.argv[1:10]
document = {
    "schemaVersion": 2,
    "mediaType": manifest_type,
    "config": {
        "mediaType": config_type,
        "digest": f"sha256:{config_digest}",
        "size": int(config_size),
    },
    "layers": [
        {
            "mediaType": media,
            "digest": f"sha256:{layer_digest}",
            "size": int(layer_size),
            "annotations": {"org.opencontainers.image.title": title},
        }
    ],
}
with open(path, "w") as handle:
    handle.write(json.dumps(document, separators=(",", ":")))
PY
  status=$(ci_tools_curl_status 30 "$registry_url/manifests/$tag" -X PUT \
    -H "Authorization: Bearer $token" -H "Content-Type: $MANIFEST_TYPE" \
    --data-binary "@$document") || status=000
  if [ "$status" != 201 ]; then
    error "$row: the manifest was PUT to $registry_url/manifests/$tag and the package answered $status where the registry API answers 201. The blob is in the package and the tag $tag names nothing, so re-run the ci-images job."
    return 1
  fi
}

held=0
filled=0
cold=0
uncopied=0
for name in $rows; do
  # The row's own second source word, read with the one reader of it. A read that fails stops
  # this run: a row whose word that reader refuses is a row nothing here can call copied,
  # filled or in need of no copy.
  second=$(ci_tools_source_name "$name" second)
  if [ "$second" != ghcr-mirror ]; then
    echo "none $name needs no copy: its second source is $second, which serves the bytes itself"
    uncopied=$((uncopied + 1))
    continue
  fi
  # The fields of a ghcr-mirror row this run fills, each read through the helper, so this
  # script learns the manifest's shape from the one reader of it.
  digest=$(ci_tools_digest "$name")
  version=$(ci_tools_field "$name" version)
  from=$(ci_tools_field "$name" mirror_from)
  member=$(ci_tools_field "$name" mirror_member)
  max_time=$(ci_tools_field "$name" max_time)
  # The copy's tag in the package, `tool-<name>-<version>-<the first eight hex characters of the
  # digest>`, so one tag names one digest and a row that moves to a new digest takes a new tag
  # with it. The tag also keeps the blob from being a package version nothing names, which a
  # retention policy may remove.
  # It is derived here and written in no row: a field that can hold one string is a field a gate
  # has to police, and the rule it would be policed against is this line.
  tag="tool-$name-$version-${digest:0:8}"
  # The address every install reads this copy at, which is the one this run writes and reads
  # back: the helper builds it, so neither side can address a blob the other does not.
  blob_url=$(ci_tools_blob_url "$name")
  tag_url="$registry_url/manifests/$tag"
  blob_status=$(registry_head "$blob_url")
  blob_size=$(header_value content-length)
  tag_status=$(registry_head "$tag_url" -H "Accept: $MANIFEST_TYPE")
  case "$blob_status" in
    200 | 404) ;;
    *)
      error "$name: a HEAD of $blob_url answered $blob_status, and this run reads 200 for a blob the package holds and 404 for one it does not. It cannot tell what this row's copy is, so it fills nothing."
      exit 1
      ;;
  esac
  case "$tag_status" in
    200 | 404) ;;
    *)
      error "$name: a HEAD of $tag_url answered $tag_status, and this run reads 200 for a tag the package resolves and 404 for one it does not. It cannot tell what this row's copy is, so it fills nothing."
      exit 1
      ;;
  esac

  # The title the copy carries: what a reader of the package sees the layer as.
  if [ -n "$member" ]; then
    media=$MEMBER_LAYER
    title=$(basename -- "$member")
  else
    media=$ARCHIVE_LAYER
    title=$(basename -- "$from")
  fi

  if [ "$blob_status" = 200 ]; then
    if [ "$tag_status" = 404 ]; then
      # The blob is there and nothing names it. A package version nothing names is one a
      # retention policy may take, and the tag is what keeps this blob.
      push_copy "$name" "$digest" "$blob_size" "$media" "$title" "$tag"
    fi
    echo "held $name sha256:$digest ($tag)"
    held=$((held + 1))
    continue
  fi

  if [ "$tag_status" = 200 ]; then
    error "$name: the package resolves $tag and holds no blob sha256:$digest, so it held this blob and does not hold it now. That is the garbage collection the tag exists to stop, and a run that filled it again would hide it. Read the package's versions for this tag, then re-run the ci-images job."
    exit 1
  fi

  asset="$work/$name.asset"
  status=0
  ci_tools_curl "$asset" "$max_time" "$(ci_tools_github_url "$from")" || status=$?
  if [ "$status" -ne 0 ]; then
    printf 'cold %s sha256:%s — %s did not answer (curl exit %s)\n' "$name" "$digest" "$from" "$status"
    printf '::warning::%s\n' \
      "mirror-ci-tools: $name has no second source yet, because $from did not answer this run and the package holds no blob sha256:$digest. Each install of this tool keeps the single source it has today until a later run of the ci-images job fills the copy." >&2
    rm -f -- "$asset"
    cold=$((cold + 1))
    continue
  fi

  bytes="$asset"
  if [ -n "$member" ]; then
    bytes="$work/$name.member"
    if ! tar -xzOf "$asset" "$member" > "$bytes"; then
      error "$name: $from answered, and its archive holds no member $member, which is the part of it this row pins. Nothing is pushed."
      exit 1
    fi
  fi
  # The one digest check this repository has, the same call every install of this copy makes on
  # what it fetched. It names the row and both digests; the URL that served the bytes is this
  # script's to add, and so is what a mismatch means here: nothing is pushed.
  if ! ci_tools_verify "$name" "$bytes" "$(ci_tools_source_name "$name" primary)"; then
    error "$name: $from served those bytes, and they are not the ones this row pins, so nothing is pushed."
    exit 1
  fi
  size=$(wc -c < "$bytes")

  # The layer is the row's own bytes, so the PUT that carries them takes the row's own
  # `max_time`, which is the bound every install reads those same bytes under.
  push_blob "$name" "$bytes" "$digest" "layer of the copy" "$max_time"
  push_copy "$name" "$digest" "$size" "$media" "$title" "$tag"

  # The one read that says the fill landed. The asset answered, so an absent blob here is a
  # defect of this run and never an outage.
  blob_status=$(registry_head "$blob_url")
  if [ "$blob_status" != 200 ]; then
    error "$name: this run pushed the blob, the config and the manifest, and a HEAD of $blob_url then answered $blob_status. The asset served the pinned bytes, so this is a defect of the fill and not an outage. Read the package for this tag, then re-run the ci-images job."
    exit 1
  fi
  echo "filled $name sha256:$digest ($tag)"
  filled=$((filled + 1))
  rm -f -- "$asset" "$work/$name.member"
done
echo "$held ghcr-mirror row(s) held, $filled filled, $cold cold, and $uncopied row(s) that need no copy"
