#!/usr/bin/env bash
# The one fetch helper for every tool .github/ci-tools.yaml names: it decides the order of the
# sources, verifies the pinned digest, and writes the log line, so no call site repeats any of it
# and no two call sites can disagree about what a refusal means.
#
# A tool this repository installs from a GitHub release asset has a second source, because an hour
# of 504s on GitHub's release assets otherwise holds every merge (dispatch://INFRA-1873). The two
# sources are not equal in rank and the two failures are not alike:
#
#   A transport failure of the primary is a reason to try the second source. Nothing is known
#   about the bytes, because there are none: a 504, a reset, a timeout. That is an outage, and the
#   second source answers it.
#
#   A digest mismatch is never a reason to try another source. The bytes arrived and they are not
#   the pinned ones, which is a supply-chain signal, not an outage. This file stops there, with
#   the source that served those bytes named, and reads nothing else.
#
#   A failure of THIS RUN is never a reason to try another source either. The one a fetch can
#   prove is a local write error: curl exits 23 where it could not write the file it was
#   handed. No second address cures a file this runner will not let a fetch write, so a fetch
#   that meets that exit refuses there and then and names no service.
#
# WHAT A REFUSAL OF THIS FILE CLAIMS: IT PROVES THE CAUSE IT CAN PROVE AND CLAIMS NOTHING ABOUT
# THE REST. One rule for the file, written here because a reader of either function below meets
# it here, and three causes a function below can prove:
#
#   A LOCAL WRITE ERROR OF THIS RUN, which curl's own exit 23 proves: this run could not write
#   the file curl was handed, so the sentence names this run and that file and says nothing
#   whatever of any source or of the bytes one serves. The shape a runner meets is a
#   destination that opens and fills part way through the write, which is ENOSPC: curl spends
#   whatever of the ladder it retries it on and answers with that same 23.
#   ci_tools_extract meets that same fault on the other side and is not refused the same way.
#   It opens its destination before it reads a byte of an archive, and the sentence it refuses
#   an unwritable one with says the same of this run — but its status is 1 and not 23, so its
#   callers report it as they report every other failure of that read, each naming a source:
#   ci_tools__primary_open prints `<word> served the bytes, and this run cannot write …`, and
#   ci_tools__second_source prints `no source served the bytes; [<primary word>: <what it
#   did>; ]<word>: this run cannot write …`, whose claim that no source served is wrong, since
#   that source did serve them. Neither is reachable where a destination is a fresh mktemp,
#   which every destination this file hands ci_tools_extract is, and the naming is the
#   caller's rather than this cause's: the rule below is of exit 23 alone.
#   A MEMBER THE ARCHIVE DOES NOT HOLD, which python3's zipfile answers for by name: the archive
#   read, and it holds no member of that name, which is a row naming a path that archive has not
#   got and a mistake in this repository.
#   DEFINITE DAMAGE OF AN ARCHIVE, which is what the archive itself raises: a zip no reader can
#   open, a member whose CRC-32 its own header no longer matches, an inflate that failed. That
#   is a signal about the bytes that address served, and the refusal says so.
#
# EVERY OTHER FAILURE IS REFUSED WITHOUT A CAUSE. The sentence says what was seen — the row, the
# address, the member, the exit the reader gave, the line it printed — and names no reason for
# it: a 504 and a reset say the bytes did not arrive and say nothing of why, `tar -xzO` answers
# an absent member, a damaged stream and a full disk with one exit 2, and python3's own exit 1
# may be the archive and may be this run. ci_tools_fetch and ci_tools_extract each say which of
# the three proofs are theirs, and neither writes this rule a second time.
#
# Every exit names the source that served the bytes — `github-release`, `ghcr-mirror`, `npm`,
# `pypi` or `pulumi-releases` — except the one curl answers with exit 23, which names none,
# because no source is what failed. ci_tools_extract's unwritable destination is that same
# fault of this run and not that exit, and its callers do name a source, as the rule above
# says. A failure leaves no file at the destination, so a call site that checks the exit
# status never runs a half-written or unverified file, and one that does not check it finds
# nothing to run.
# Those five words are the whole vocabulary of a source, and a row writes one of them on each of
# its two sides, in `primary_source` and in `second_source`, so a line, a refusal and the
# manifest say one word for one source. A call site takes the word from the row with
# ci_tools_source_name below and writes none of its own: a row that moved to another source
# while an action held the old word would name a service that served no byte of what it
# installed.
# ci_tools__source_fetch below is this file's one list of those words, and it is read for both
# sides, so no word is printed for a row and then refused when the bytes are fetched. It is not
# the tree's only list, and the others are named there, at the case itself, so this file keeps
# no count of its own. What each word reads here is the row's own fields for the side the word
# is written on:
#
#   github-release          `primary` or `second_url`, through the CI_TOOLS_GITHUB_BASE seam.
#                           It is most rows' primary, and it is a second source as well: Pulumi
#                           publishes the CLI tarball as a release asset and serves those same
#                           bytes from get.pulumi.com.
#   pypi, pulumi-releases   the same two URL fields, through the CI_TOOLS_URL_BASE seam. Two
#                           words for the one fetch ci_tools__fetch_url makes, because the word
#                           is the service a line and a refusal name and PyPI is not
#                           get.pulumi.com: PyPI serves uv's wheel, get.pulumi.com serves the
#                           docker provider plugin's tarball and the CLI's.
#   ghcr-mirror             the row's blob in the package `registry:` names.
#   npm                     the row's `npm_package` and `npm_member`.
#
# `ghcr-mirror` and `npm` are second sources and no row's primary — a copy this repository fills
# is no origin of the bytes, and npm is where Bun publishes a second copy of a binary GitHub
# serves first — so a row whose `primary_source` names one is refused by the same sentence that
# refuses a word this file has never heard of, with the side it was written on named.
#
# Two more words a line may say and no row may hold: `cache` and `existing-install`.
# .github/actions/setup-bun writes both itself, where it installed nothing and read what
# oven-sh/setup-bun left at ~/.bun/bin/bun: `cache` where that action restored Bun from this
# repository's Actions cache instead of downloading the release asset, and `existing-install`
# where a Bun of the pinned version was already at that path when the action started and that
# action kept it, which it reports as a cache hit too. Neither names a store an install can
# fetch a tool from — the one holds bytes a row already pins, the other is the runner's own
# filesystem — so neither is a property of a row, no `second_source:` may name either, and
# nothing in this file prints them.
#
# THIS FILE IS SOURCED, NEVER EXECUTED. It defines functions, declares the empty cache those
# functions keep a read manifest row in, reads nothing and fetches nothing at load, and it is
# not executable (0644). It carries no `set -euo pipefail`, because that would change the shell of
# the caller that sourced it; each function is written to be safe on its own, under a caller that
# has errexit, nounset and pipefail on, as a GitHub `run:` step does, and under one that has none
# of them.
#
#   source "$GITHUB_ACTION_PATH/../../scripts/ci-tools.sh"
#   ci_tools_fetch crane "$RUNNER_TEMP/go-containerregistry_Linux_x86_64.tar.gz"
#
# THE PUBLIC FUNCTIONS.
#
#   ci_tools_field <row> <field>          one field of that row of the manifest, and the
#                                         manifest's own `registry` under that name. A field
#                                         the manifest leaves null reads as an empty string.
#   ci_tools_digest <row>                 that row's pinned digest.
#   ci_tools_source_name <row>
#                   <primary|second>      the one word that source is named by, for a call site
#                                         that must write no source word of its own.
#   ci_tools_rows [<second source>]       every row's name, in the manifest's own order, and
#                                         only the rows whose second source is that word where
#                                         the argument is given.
#   ci_tools_github_url <url>             that URL with the CI_TOOLS_GITHUB_BASE seam in place
#                                         of its https://github.com prefix.
#   ci_tools_curl <file> <max_time>
#                 <url> [argument…]       the one fetch this file makes: <url> into <file>,
#                                         under the retry ladder and the bounds every source is
#                                         read with. .github/scripts/mirror-ci-tools.sh fetches
#                                         the asset it pushes with it, so the copy is filled
#                                         with the same ladder an install reads it under.
#   ci_tools_extract <archive> <member>
#                    <destination> <url>  that member of that archive at <destination>, read by
#                                         the suffix of the address it came from rather than by
#                                         its own path: a `.whl` or a `.zip` with python3's
#                                         zipfile and anything else with `tar -xzO`. It is the
#                                         one place this repository takes a member out of an
#                                         archive: the npm fetch and both member fields below
#                                         read theirs with it, and
#                                         .github/scripts/ci-tools-proof.sh takes the row's
#                                         pinned member out of the archive it staged with
#                                         ci_tools_fetch_primary rather than fetching that
#                                         archive a second time.
#   ci_tools_blob_url <row>               that row's blob in the package `registry:` names: the
#                                         address a ghcr-mirror row's second source is read
#                                         from, and the one .github/scripts/mirror-ci-tools.sh
#                                         writes and reads back.
#   ci_tools_registry                     the package `registry:` names, printed, for the one
#                                         line a run writes about which package it read.
#   ci_tools_registry_url                 the registry API address of that package,
#                                         <CI_TOOLS_GHCR_BASE>/v2/<its path>. It is the one
#                                         owner of that address: ci_tools_blob_url and every
#                                         URL .github/scripts/mirror-ci-tools.sh pushes to and
#                                         reads back are built from it.
#   ci_tools_token_of <file>              the `.token` of a registry token response. It is the
#                                         one reader of such an answer this repository has, and
#                                         ci_tools_ghcr_token below reads every token with it.
#   ci_tools_ghcr_token [<max_time>
#                        [<scope>
#                         [<credential>]]]
#                                         a token for the package `registry:` names. With no
#                                         argument it is the anonymous `pull` token a read of a
#                                         copy carries; .github/scripts/mirror-ci-tools.sh asks
#                                         for `pull,push` with GITHUB_ACTOR:GITHUB_TOKEN. An
#                                         empty argument takes that position's default.
#   ci_tools_verify <row> <file> <source> holds <file> to that digest and writes the line that
#                                         names <source>. It is what an action calls to hold an
#                                         installed binary to its row, so it leaves the file where
#                                         it is, whichever way it answers.
#   ci_tools_fetch <row> <dest>           the primary first, then the second source — and no
#                                         second source after a local write error of this run,
#                                         which no other address cures. Where the row names a
#                                         `primary_member`, what reaches <dest> is that member
#                                         of the primary's archive.
#   ci_tools_fetch_primary <row> <dest>   the primary alone, and the archive rather than the
#                                         member: the bytes that URL serves, held to the row's
#                                         `primary_digest` where it names one and to its own
#                                         `digest` where it does not. It reads no second source.
#                                         .github/scripts/ci-tools-proof.sh stages a row's
#                                         primary bytes with it, which for a row whose `digest`
#                                         is of a member is not the file ci_tools_fetch leaves.
#   ci_tools_fetch_fallback <row> <dest>  the second source alone, for a row whose primary bytes a
#                                         third party fetches: setup-bun fetches Bun's zip and
#                                         mise fetches jj's tarball, so there is no primary here
#                                         to try.
#
# THE FIELDS OF A ROW THOSE FETCHES READ, beyond the `digest` and the `max_time` every row
# carries:
#
#   primary_source     the word the row's primary is read with, which is the word every line and
#                      every refusal of it says. Every row carries it, a row whose primary bytes
#                      a third party fetches included, since that row's action says this word
#                      about the bytes it installed.
#   primary            the URL that source serves the primary's bytes at, and "" where a third
#                      party fetches them, which sends ci_tools_fetch straight to the second
#                      source.
#   second_source      the word the row's second source is read with.
#   second_url         where that source serves this row's bytes, written whole, because a
#                      service has no one prefix every row of it begins with the way a release
#                      asset has https://github.com.
#   second_member      the member to take out of what `second_url` served, and "" to keep the
#                      whole file. A `.whl` or a `.zip` is read with python3's zipfile and
#                      anything else with `tar -xzO`, as the npm fetch reads its tarball.
#   primary_digest     the sha256 of the bytes the PRIMARY URL serves, for the row whose own
#                      `digest` is of something inside them: uv's `digest` is of the binary,
#                      which PyPI's wheel and GitHub's tarball each hold, so the archive GitHub
#                      serves has a digest of its own and this is it. A mismatch on it is the
#                      supply-chain signal a mismatch on `digest` is, and the second source is
#                      not read after one. "" where the primary serves exactly the bytes the row
#                      pins, which is every other row.
#   primary_digest_of  the prose saying what `primary_digest` is of, which the refusal of a
#                      mismatch on it says. "" where unused.
#   primary_member     the member to take out of the primary's archive, by the suffix rule
#                      above, before the row's own `digest` is checked, so the digest answers
#                      for the member and the archive reaches no destination. "" where unused.
#
# THE FIVE SEAMS, each read with a default, so a CI job sets none of them:
#
#   CI_TOOLS_MANIFEST      the manifest. Default: the ci-tools.yaml beside this file's directory,
#                          resolved from ${BASH_SOURCE[0]}, so a composite action that sources
#                          this file by any path finds the manifest without naming it.
#   CI_TOOLS_GITHUB_BASE   default https://github.com. The URL a `github-release` side of a row
#                          writes — its `primary`, its `second_url` — and a row's `mirror_from`
#                          begin with that string, and this file replaces exactly that prefix.
#   CI_TOOLS_GHCR_BASE     default https://ghcr.io, read by ci_tools__ghcr alone, so every
#                          address of the package is built the one way.
#   CI_TOOLS_NPM_BASE      default https://registry.npmjs.org.
#   CI_TOOLS_URL_BASE      default empty, which fetches the URL a `pypi` or `pulumi-releases`
#                          side of a row writes exactly as the row wrote it. Set, it replaces
#                          that URL's scheme and host and keeps its path, so a row written at
#                          files.pythonhosted.org whose path is /packages/u/uv.whl is fetched
#                          under CI_TOOLS_URL_BASE=http://127.0.0.1:9/url at
#                          http://127.0.0.1:9/url/packages/u/uv.whl. The host is named here
#                          without a path after it because rule 3 of
#                          .github/scripts/check-ci-tools.sh reads every file under .github/
#                          as text and refuses a whole address on a host a row names, this
#                          file included. It replaces a scheme and a
#                          host rather than one literal prefix, because every row of such a
#                          source writes an address of its own. Read by ci_tools__seamed_url
#                          alone, so every such URL is fetched the one way.
#
# .github/scripts/ci-tools.test.sh serves all four sources from a local HTTP origin through
# those five seams, so every case runs this file's own code and no case reaches the network.

# ci_tools__error <sentence>: a refusal, as a GitHub annotation on stderr.
ci_tools__error() {
  printf '::error::ci-tools: %s\n' "$*" >&2
}

# The two exit codes ci_tools_extract's zip reader answers with, for the two causes it can
# PROVE: a member the archive does not hold, and damage the archive itself raised. Neither is
# 1, so python3's own exit 1 — every other exception, which may be the archive and may be this
# run and which this file therefore refuses without clearing the bytes — stays a code of its
# own, and the three are told apart by these two numbers rather than by reading a message. The
# reader is handed both as arguments, so these lines are the one place either is written.
ci_tools__no_such_member=3
ci_tools__damaged_archive=4

# curl's CURLE_WRITE_ERROR: the exit it answers with where it could not write the file it was
# given, which is the one cause of a fetch this file can prove. A runner with no room left is
# the live shape of it — .github/workflows/ci.yaml samples disk space around the envoy-infra
# live tests because runners fill up — and a fetch that meets it reads no second source, since
# no other address serves a file this runner will not let it write.
# ci_tools__curl_report below writes the one sentence for it and hands this number back, and
# ci_tools_fetch, ci_tools__second_source and ci_tools_fetch_primary read it to refuse.
ci_tools__write_error=23

# ci_tools__manifest: the manifest this call reads.
ci_tools__manifest() {
  if [ -n "${CI_TOOLS_MANIFEST:-}" ]; then
    printf '%s\n' "$CI_TOOLS_MANIFEST"
    return 0
  fi
  local here
  here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd) || return 1
  printf '%s\n' "$here/../ci-tools.yaml"
}

# The rows this process has already read, keyed by the manifest's path and the row's name, and
# the fields of the row the last read answered with.
# It is the memory of one process and not a file: a cache on disk would be one more thing that
# can be stale, and that objection stands.
declare -gA ci_tools__row_cache=()
ci_tools__row_fields=''

# ci_tools__row <row>: every field of that row as `<field><TAB><value>` lines in
# ci_tools__row_fields, with the manifest's own `registry` among them, since a ghcr row's second
# source is a blob in that package.
# A successful read is kept for the rest of this process: the file is small, but a full PyYAML
# parse costs about 95 ms and one job makes a hundred of these calls.
# Only a successful read is kept, so a refusal is produced and printed every time it is asked
# for, and the manifest's own path is part of the key, so a caller that moves the
# CI_TOOLS_MANIFEST seam reads the file it named.
# It answers in a variable and not on stdout because a command substitution is a process of its
# own: the row a reader read inside one is a row the shell that asked for it never learns.
ci_tools__row() {
  local row=$1 manifest key
  manifest=$(ci_tools__manifest) || return 1
  key="$manifest"$'\t'"$row"
  if [ -n "${ci_tools__row_cache[$key]+held}" ]; then
    ci_tools__row_fields=${ci_tools__row_cache[$key]}
    return 0
  fi
  ci_tools__row_fields=$(CI_TOOLS_ROW="$row" CI_TOOLS_FILE="$manifest" PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import os
import sys

import yaml

path = os.environ["CI_TOOLS_FILE"]
want = os.environ["CI_TOOLS_ROW"]
try:
    with open(path) as handle:
        document = yaml.safe_load(handle) or {}
except OSError as error:
    sys.stderr.write(f"::error::ci-tools: {want}: this run cannot read {path} ({error})\n")
    raise SystemExit(1)
rows = {row.get("name"): row for row in document.get("tools") or []}
row = rows.get(want)
if row is None:
    held = ", ".join(sorted(name for name in rows if name)) or "no row at all"
    sys.stderr.write(
        f"::error::ci-tools: {want}: {path} holds no row of that name (it holds: {held})\n"
    )
    raise SystemExit(1)
registry = document.get("registry") or ""
print(f"registry\t{registry}")
# A field the manifest leaves null reads as an empty string, which is the reading
# .github/scripts/check-ci-tools.sh already has of `primary:` with no value and the reading
# every caller here is written for. Python's own `None` would reach a caller as the word None:
# an install would fetch the URL `None` through the whole retry ladder before it read the
# second source, and the filler would ask tar for the member `None`.
for field, value in row.items():
    print(f"{field}\t{'' if value is None else value}")
PY
  ) || return 1
  ci_tools__row_cache[$key]=$ci_tools__row_fields
  return 0
}

# ci_tools__read <variable> <row> <field>: that field of that row into <variable>, which is how
# every reader inside this file asks for one. No command substitution stands between the caller
# and the cache above, so the second field of a row this process already read costs nothing.
ci_tools__read() {
  local __variable=$1 __row=$2 __field=$3 __key __value __held=''
  ci_tools__row "$__row" || return 1
  while IFS=$'\t' read -r __key __value; do
    if [ "$__key" = "$__field" ]; then
      printf -v "$__variable" '%s' "$__value"
      return 0
    fi
    __held="$__held${__held:+, }$__key"
  done <<<"$ci_tools__row_fields"
  ci_tools__error "$__row: this row of the manifest carries no field '$__field' (it carries: $__held)"
  return 1
}

# ci_tools_field <row> <field>: one field of that row, printed. It is what a call site outside
# this file reads a row with.
ci_tools_field() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_field takes a row and a field, and this call gave $#"
    return 1
  fi
  local value
  ci_tools__read value "$1" "$2" || return 1
  printf '%s\n' "$value"
}

# ci_tools_source_name <row> <primary|second>: the one word that source of that row is named by,
# which is the word its log line and its refusal say.
# It is what .github/actions/install-jj and .github/actions/setup-bun read their two source words
# from, so no action holds a source word of its own and a row that moves to another second source
# moves every sentence about it with one edit.
#
# `second` is the row's own `second_source` and `primary` is its own `primary_source`, and a
# word ci_tools__source_fetch below reads no source of that side with is refused rather than
# printed: that one case decides which words each side of a row is written in, so a word this
# file would name and then refuse to fetch cannot exist. Adding a source is an edit in each of
# the places ci_tools__source_fetch below names, which that one comment lists.
#
# Neither side is a constant. Most rows' primary is a GitHub release asset and the pulumi CLI's
# is get.pulumi.com, and a third party fetches the primary bytes of jj (mise) and of Bun
# (oven-sh/setup-bun), which is why those rows carry an empty `primary` and a `primary_source`
# all the same: the word is what their action says about bytes it did not fetch here. The row is
# read either way, so a call site that names no row of the manifest is refused here rather than
# answered with a word about a tool this file knows nothing of.
ci_tools_source_name() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_source_name takes a row and primary or second, and this call gave $#"
    return 1
  fi
  local row=$1 which=$2 word fetch
  case "$which" in
    primary) ci_tools__read word "$row" primary_source || return 1 ;;
    second) ci_tools__read word "$row" second_source || return 1 ;;
    *)
      # The row first, so a call that names no row of the manifest is refused by its own name.
      ci_tools__row "$row" || return 1
      ci_tools__error "ci_tools_source_name reads the primary or the second source of a row, and this call asked for '$which'"
      return 1
      ;;
  esac
  # The word is printed only where a fetch of that side answers to it, and which function that
  # is has no use here: reading it is the whole check.
  ci_tools__source_fetch "$row" "$which" "$word" fetch || return 1
  printf '%s\n' "$word"
}

# ci_tools_digest <row>: that row's pinned digest.
ci_tools_digest() {
  if [ "$#" -ne 1 ]; then
    ci_tools__error "ci_tools_digest takes a row, and this call gave $#"
    return 1
  fi
  ci_tools_field "$1" digest
}

# ci_tools_rows [<second source>]: every row's name, in the manifest's own order, and only the
# rows whose `second_source` is that word where the argument is given. One reader of the
# manifest's row list, so a job's step names neither the file nor its shape.
ci_tools_rows() {
  if [ "$#" -gt 1 ]; then
    ci_tools__error "ci_tools_rows takes an optional second source, and this call gave $#"
    return 1
  fi
  local manifest names
  manifest=$(ci_tools__manifest) || return 1
  names=$(CI_TOOLS_SECOND="${1:-}" CI_TOOLS_FILE="$manifest" PYTHONDONTWRITEBYTECODE=1 python3 - <<'PY'
import os
import sys

import yaml

path = os.environ["CI_TOOLS_FILE"]
want = os.environ["CI_TOOLS_SECOND"]
try:
    with open(path) as handle:
        document = yaml.safe_load(handle) or {}
except OSError as error:
    sys.stderr.write(f"::error::ci-tools: this run cannot read {path} ({error})\n")
    raise SystemExit(1)
for row in document.get("tools") or []:
    name = row.get("name")
    if not name:
        continue
    if want and row.get("second_source") != want:
        continue
    print(name)
PY
  ) || return 1
  [ -z "$names" ] || printf '%s\n' "$names"
}

# ci_tools__hold <row> <file> <source> <digest> <what that digest is of>: holds <file> to that
# digest, saying nothing where it holds and refusing where it does not. It is the one digest
# comparison this file makes: ci_tools__verify_as below answers a row's own `digest` with it and
# ci_tools__primary_open answers a row's `primary_digest` with it, so no two checks of this
# repository can disagree about what a match is or about what a mismatch says.
# The fifth argument is the prose saying what the digest is of, which the refusal names. It is
# empty where the digest is of the bytes themselves, which is every row's own `digest`, and the
# sentence then carries no clause.
ci_tools__hold() {
  local row=$1 file=$2 source=$3 want=$4 of=$5 read_back
  read_back=$(sha256sum -- "$file") || {
    ci_tools__error "$row: $source left no file this run can read at $file — refusing"
    return 1
  }
  read_back=${read_back%% *}
  if [ "$read_back" != "$want" ]; then
    ci_tools__error "$row: the bytes from $source are not the pinned ones${of:+ for $of} (pinned sha256 $want, read sha256 $read_back) — refusing"
    return 1
  fi
  return 0
}

# ci_tools__verify_as <row> <file> <source> <digest> <what that digest is of>: that comparison
# and the line that names <source>. ci_tools_verify below answers a row's own `digest` with it,
# ci_tools__place writes the line of every fetch with it, and ci_tools_fetch_primary names the
# primary's own digest through it, so one spelling of that line serves every exit.
ci_tools__verify_as() {
  local row=$1 file=$2 source=$3 want=$4 of=$5
  ci_tools__hold "$row" "$file" "$source" "$want" "$of" || return 1
  printf 'ci-tools: %s: %s served the bytes, sha256 %s\n' "$row" "$source" "$want"
}

# ci_tools_verify <row> <file> <source>: holds <file> to the row's digest, and says so either way.
# It never deletes the file: an action calls it to hold an installed binary to its row, and the
# caller that fetched the file is the one that owns it.
ci_tools_verify() {
  if [ "$#" -ne 3 ]; then
    ci_tools__error "ci_tools_verify takes a row, a file and a source, and this call gave $#"
    return 1
  fi
  local row=$1 file=$2 source=$3 want
  ci_tools__read want "$row" digest || return 1
  ci_tools__verify_as "$row" "$file" "$source" "$want" ''
}

# ci_tools__tempfile <row>: a file under the runner's own temporary directory. Every path below
# that does not move it to the destination removes it.
ci_tools__tempfile() {
  local row=$1 directory
  directory=${RUNNER_TEMP:-${TMPDIR:-/tmp}}
  mktemp "$directory/ci-tools-${row//[^A-Za-z0-9._-]/-}.XXXXXX"
}

# ci_tools_curl <file> <max_time> <url> [curl argument…]: the one fetch this file makes, and the
# one .github/scripts/mirror-ci-tools.sh makes for the asset it copies, so the bytes a copy is
# filled with are fetched under the ladder an install reads them under.
# `--fail` is what keeps a 504's body out of <file>, `--retry 7 --retry-all-errors` rides out a
# flap of the service, and `--max-time` bounds one attempt, so a hung attempt becomes the exit
# code that sends the caller to the second source. Exit 23 is the one exit that sends it
# nowhere: ci_tools__curl_report below reads it as this run's own write error, and the caller
# refuses where it stands.
#
# `--retry-max-time 60` bounds the ladder as a whole, and it is the bound on when a new attempt
# may start: an attempt already running when the minute is over still finishes under
# `--max-time`, and curl starts no attempt after it.
# What it buys is the hang. Without it a host that accepts and sends nothing costs the whole
# ladder — eight attempts of `--max-time` 60 s plus a back-off of 1, 2, 4, 8, 16, 32 and 64 s —
# which is about ten minutes for one row, while many jobs of .github/workflows/ci.yaml carry
# `timeout-minutes: 10`.
# Against a source that answers 504 at once, the shape of the 2026-11-08 outage, it buys almost
# nothing: the back-off alone reaches the minute, so the ladder is one attempt shorter.
# This issue's tester measured both shapes at the manifest's `max_time` of 60. Against a source
# that refuses at once, as a 504 does, the bound makes 7 attempts. Their back-off is
# 1+2+4+8+16+32, so that ladder costs 63 s. Without the bound the same source makes 8 attempts,
# which is that back-off plus 64 s, or 127 s. The 63 s was measured twice. One such ladder
# against a port that refuses at once took 1m3.064s and exited 7. And in job 129743677408 of
# .github/workflows/ci.yaml the two kinds of step are apart by a factor of two: the eight
# negative controls, which each drive two ladders because their primary meets the fault and
# their second source is a dead port, measured 104 to 127 s, while the head's own eight
# installs and the eight second-source cases, which each drive one, measured about 64 s. So
# 126 s is two ladders and not one. No negative control of that job drives a single ladder.
# The hang is the other shape. It costs 124 s with the bound, where the ladder without the
# bound costs about ten minutes. So a row's primary costs about one minute before the second
# source is read against a source that refuses at once, and about two minutes against a host
# that hangs. Both are inside every ci.yaml budget.
ci_tools_curl() {
  local file=$1 max_time=$2 url=$3
  shift 3
  curl -fsSL --retry 7 --retry-all-errors --retry-max-time 60 --max-time "$max_time" -o "$file" "$@" "$url"
}

# ci_tools__curl_report <curl's exit> <the file curl was given> <what every other exit says>:
# the sentence that exit is reported in, printed, and the status the caller returns with it.
# It is this file's one reader of a curl exit, so the four fetches that make one — the
# primary's, the second source's, the ghcr blob's with the token request before it, and the
# npm tarball's — say one thing about a local write error.
# Exit 23 is curl's own, and the one cause a fetch can prove: this run could not write that
# file, and the sentence says that and nothing of any source or the bytes it serves. The status
# is that 23, which a caller reads to refuse rather than fetch again elsewhere.
# Every other exit keeps the sentence its caller wrote and returns 1: the bytes did not arrive,
# and nothing here says why.
ci_tools__curl_report() {
  local status=$1 file=$2 otherwise=$3
  if [ "$status" -eq "$ci_tools__write_error" ]; then
    printf 'this run could not write %s (curl exit %s), which says nothing about any source or the bytes it serves\n' \
      "$file" "$status"
    return "$ci_tools__write_error"
  fi
  printf '%s\n' "$otherwise"
  return 1
}

# ci_tools_github_url <url>: that URL with the CI_TOOLS_GITHUB_BASE seam in place of its
# https://github.com prefix. .github/scripts/mirror-ci-tools.sh reads a row's `mirror_from`
# through it, so both readers of a row's URL apply that seam the one way.
ci_tools_github_url() {
  local url=$1
  case "$url" in
    https://github.com/*) printf '%s\n' "${CI_TOOLS_GITHUB_BASE:-https://github.com}${url#https://github.com}" ;;
    *) printf '%s\n' "$url" ;;
  esac
}

# ci_tools__seamed_url <url>: that URL with the CI_TOOLS_URL_BASE seam in place of its scheme and
# host, and the URL as it was written where that seam is empty, which is every CI job. It is the
# one reader of that seam, as ci_tools__ghcr is of CI_TOOLS_GHCR_BASE.
# It replaces a scheme and a host where ci_tools_github_url replaces one literal prefix, because
# a row's `second_url` is the whole address one service serves that row's bytes at and no one
# prefix every such row begins with: PyPI's host is not get.pulumi.com's.
ci_tools__seamed_url() {
  local url=$1 base=${CI_TOOLS_URL_BASE:-} path
  if [ -z "$base" ]; then
    printf '%s\n' "$url"
    return 0
  fi
  path=${url#*://}
  case "$path" in
    */*) path=/${path#*/} ;;
    *) path='' ;;
  esac
  printf '%s%s\n' "${base%/}" "$path"
}

# ci_tools__place <row> <file> <dest> <source> [<digest> [<what that digest is of>]]: the one end
# of a successful fetch. It verifies the digest before the bytes reach <dest>, so a mismatch
# leaves nothing there, and it removes <file> on every path that does not move it.
# With no digest it is the row's own, which is the digest every fetch of a row's bytes places
# them under. ci_tools_fetch_primary names the primary's own digest instead, because the archive
# it places is not the file that row's `digest` is of.
ci_tools__place() {
  local row=$1 file=$2 dest=$3 source=$4 digest=${5:-} of=${6:-}
  if [ -z "$digest" ] && ! ci_tools__read digest "$row" digest; then
    rm -f -- "$file"
    return 1
  fi
  if ! ci_tools__verify_as "$row" "$file" "$source" "$digest" "$of"; then
    rm -f -- "$file"
    return 1
  fi
  if ! mv -f -- "$file" "$dest"; then
    rm -f -- "$file"
    ci_tools__error "$row: $source served the pinned bytes, and this run could not put them at $dest — refusing"
    return 1
  fi
  return 0
}

# ci_tools_token_of <file>: the `.token` of the JSON a registry's token endpoint answered, and
# nothing where that file holds no readable one. One reader of a token response: this file asks
# for an anonymous pull token and .github/scripts/mirror-ci-tools.sh for a pull,push one, and
# both read the answer here.
ci_tools_token_of() {
  PYTHONDONTWRITEBYTECODE=1 python3 -c '
import json
import sys

with open(sys.argv[1]) as handle:
    print(json.load(handle)["token"])
' "$1" 2>/dev/null
}

# ci_tools_ghcr_token [<max_time> [<scope> [<credential>]]]: a token for the package the
# manifest's `registry:` names. Each argument has a default and an empty one takes it, so a
# caller that names a credential alone writes `ci_tools_ghcr_token "" pull,push "$user:$secret"`.
#
# The defaults are what an install carries: the package is public to read, so a `pull` token is
# minted with no credential at all, and ghcr.io answers one for it and refuses a blob without
# it. .github/scripts/mirror-ci-tools.sh writes to that same package, so it asks for
# `pull,push` with GITHUB_ACTOR:GITHUB_TOKEN, and the one request both make is this one.
#
# Prints the token, and prints what happened and returns non-zero where the request or the read
# of its answer failed, as the fetch functions below do, so a caller holds the sentence rather
# than a half-read token. A request this run could not write the answer to returns the write
# error itself.
ci_tools_ghcr_token() {
  if [ "$#" -gt 3 ]; then
    ci_tools__error "ci_tools_ghcr_token takes an optional per-attempt bound, scope and credential, and this call gave $#"
    return 1
  fi
  local max_time=${1:-60} scope=${2:-pull} credential=${3:-}
  local base path url token_file token status=0
  # A credential is passed to curl as its own argument and never spliced into the URL, so the
  # secret reaches no log line and no process name of a redirect.
  local -a authentication=()
  [ -z "$credential" ] || authentication=(-u "$credential")
  ci_tools__ghcr base path || return 1
  token_file=$(ci_tools__tempfile ghcr-token) || {
    printf 'this run could not make a temporary file for the %s token\n' "$scope"
    return 1
  }
  url="$base/token?service=ghcr.io&scope=repository:$path:$scope"
  ci_tools_curl "$token_file" "$max_time" "$url" \
    ${authentication[@]+"${authentication[@]}"} || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$token_file"
    ci_tools__curl_report "$status" "$token_file" \
      "the $scope token request failed (curl exit $status)"
    return
  fi
  token=$(ci_tools_token_of "$token_file") || token=''
  rm -f -- "$token_file"
  if [ -z "$token" ]; then
    printf 'the %s token request answered no .token this run could read\n' "$scope"
    return 1
  fi
  printf '%s\n' "$token"
}

# ci_tools__registry <variable>: the package every ghcr copy lives in, which the manifest names
# once for the whole file, into that variable. Every row carries it, so the first row answers
# for all of them.
ci_tools__registry() {
  local __variable=$1 first
  first=$(ci_tools_rows) || return 1
  first=${first%%$'\n'*}
  if [ -z "$first" ]; then
    ci_tools__error "the manifest holds no row, so this run cannot read the package the copies live in"
    return 1
  fi
  ci_tools__read "$__variable" "$first" registry
}

# ci_tools_registry: the package every ghcr copy lives in, printed. It is what a call site says
# in the one line that names the package it read or wrote.
ci_tools_registry() {
  if [ "$#" -ne 0 ]; then
    ci_tools__error "ci_tools_registry takes no argument, and this call gave $#"
    return 1
  fi
  local registry
  ci_tools__registry registry || return 1
  printf '%s\n' "$registry"
}

# ci_tools__ghcr <base variable> <path variable>: the CI_TOOLS_GHCR_BASE seam and the package's
# path under it, which is the manifest's `registry:` with its host taken off, into those two
# variables. It is the one place either is written: the token request above and the address
# below are built from it, so no reader of the package holds a spelling of its own for where
# that package is.
ci_tools__ghcr() {
  local __base=$1 __path=$2 registry
  ci_tools__registry registry || return 1
  printf -v "$__base" '%s' "${CI_TOOLS_GHCR_BASE:-https://ghcr.io}"
  printf -v "$__path" '%s' "${registry#*/}"
}

# ci_tools_registry_url: the registry API address of that package, <base>/v2/<path>. Every URL
# of the package is this address and what follows it: ci_tools_blob_url below is this and the
# row's digest, and .github/scripts/mirror-ci-tools.sh's uploads, manifests and read-backs are
# this and theirs, so the filler of a copy and every reader of one address one package.
ci_tools_registry_url() {
  if [ "$#" -ne 0 ]; then
    ci_tools__error "ci_tools_registry_url takes no argument, and this call gave $#"
    return 1
  fi
  local base path
  ci_tools__ghcr base path || return 1
  printf '%s/v2/%s\n' "$base" "$path"
}

# ci_tools_blob_url <row>: the row's blob in the package the manifest's `registry:` names. It is
# the address of a ghcr-mirror row's second source, which is the row's own pinned digest: the
# fetch below reads it, and .github/scripts/mirror-ci-tools.sh writes it and reads it back, so
# neither of the two can address a blob the other does not.
ci_tools_blob_url() {
  if [ "$#" -ne 1 ]; then
    ci_tools__error "ci_tools_blob_url takes a row, and this call gave $#"
    return 1
  fi
  local row=$1 digest registry_url
  # The row first, so a call that names no row of the manifest is refused by its own name
  # rather than by the package it has no row in.
  ci_tools__read digest "$row" digest || return 1
  registry_url=$(ci_tools_registry_url) || return 1
  printf '%s/blobs/sha256:%s\n' "$registry_url" "$digest"
}

# ci_tools__fetch_ghcr <row> <side> <max_time> <file>: the row's blob from the copy in the
# package the manifest's `registry:` names, read with the anonymous pull token above. The blob
# is already the bytes the digest names, so nothing is taken out of it.
# The side is the second argument of every source fetch and this one does not read it: the list
# in ci_tools__source_fetch reads `ghcr-mirror` as a second source and no row's primary.
# Prints what happened, and returns non-zero, where the service did not serve the bytes, and
# returns the write error itself where this run is what could not write the token or the blob.
ci_tools__fetch_ghcr() {
  local row=$1 max_time=$3 file=$4
  local url token status=0
  url=$(ci_tools_blob_url "$row") || return 1
  # The token helper prints what went wrong in place of a token, and this is the caller that
  # reports it: the sentence belongs in the refusal this row's fetch builds, and its status
  # carries a write error of that request back unread.
  token=$(ci_tools_ghcr_token "$max_time") || status=$?
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$token"
    return "$status"
  fi
  ci_tools_curl "$file" "$max_time" "$url" -H "Authorization: Bearer $token" || status=$?
  if [ "$status" -ne 0 ]; then
    ci_tools__curl_report "$status" "$file" "the blob request failed (curl exit $status)"
    return
  fi
  return 0
}

# ci_tools__fetch_npm <row> <side> <max_time> <file>: the row's binary out of the npm package
# upstream publishes it in. npm needs no copy of ours: it is a service every job already
# reaches, and it holds the same binary upstream puts in the GitHub zip.
# The side goes unread here for the reason it goes unread above: `npm` is a second source and
# no row's primary.
# The member is taken out with ci_tools_extract below, which the tarball's own `.tgz` address
# sends to `tar -xzO`: one extractor for every archive this file reads, so a tarball that does
# not give that member up is refused in the sentence that names the member and the address it
# came from.
# Prints what happened, and returns non-zero, where the service did not serve the bytes, and
# returns the write error itself where this run is what could not write the tarball.
ci_tools__fetch_npm() {
  local row=$1 max_time=$3 file=$4
  local base=${CI_TOOLS_NPM_BASE:-https://registry.npmjs.org}
  local package version member tarball url report status=0
  ci_tools__read package "$row" npm_package || return 1
  ci_tools__read version "$row" version || return 1
  ci_tools__read member "$row" npm_member || return 1
  tarball=$(ci_tools__tempfile "$row-npm") || {
    printf 'this run could not make a temporary file for the package tarball\n'
    return 1
  }
  # npm serves a scoped package's tarball under the package name without its scope.
  url="$base/$package/-/${package##*/}-$version.tgz"
  ci_tools_curl "$tarball" "$max_time" "$url" || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$tarball"
    ci_tools__curl_report "$status" "$tarball" \
      "the package tarball request failed (curl exit $status)"
    return
  fi
  report=$(ci_tools_extract "$tarball" "$member" "$file" "$url") || status=$?
  rm -f -- "$tarball"
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$report"
    return 1
  fi
  return 0
}

# ci_tools_extract <archive> <member> <destination> <url>: that member of that archive at
# <destination>. The address the archive came from decides how it is read, and not the archive's
# own path, which is a temporary file of this run and carries no suffix: a `.whl` or a `.zip`
# with python3's zipfile, which this file already reads every row of the manifest with, and
# anything else with `tar -xzO`, which is how the npm fetch above reads its tarball.
# Prints what happened, and returns non-zero, where the member did not come out, so the caller
# refuses by the member and the archive rather than by the digest of an empty file. A failure
# leaves nothing at <destination>, the bytes a reader had already written there included, so the
# promise this file's header makes holds for a caller that hands this function the destination
# itself and not a temporary file of its own.
# OF THE THREE CAUSES THIS FILE'S HEADER NAMES, THIS FUNCTION PROVES ALL THREE, one of them
# before it reads anything and the other two out of what the zip arm's reader says. Each arm
# taught the other how far that reaches:
#
#   A destination this run cannot even open is the local write error on this side, and it is
#   proved by the pre-check both arms share, before a byte of either archive is read. bash
#   says on stderr why it could not be opened, and the sentence claims nothing about the
#   archive because nothing of the archive has been read.
#   A member the archive does not hold is the zip arm's own proof: the archive read, and it
#   holds no member of that name. `tar -xzO` answers that and a damaged stream with the same
#   exit 2 — one changed byte breaks the gzip framing — so the tar arm never could name a
#   cause and says none.
#   What the ARCHIVE raises — zipfile.BadZipFile, zlib.error, EOFError: a zip that cannot be
#   read at all, a member whose CRC-32 the archive's own header no longer matches, an inflate
#   that failed, a stream that ended early — is definite damage, which is the supply-chain
#   signal this file's header is about, and the sentence says so and quotes python3's last
#   line.
#
# WHAT IS LEFT IS EVERY OTHER EXCEPTION OF THE ZIP ARM, and that vocabulary does not partition
# cause: python3 raises OSError on a damaged member's stream and on the destination alike, and
# a tampered central directory reaches it as RuntimeError (an encrypted flag it never set),
# NotImplementedError (a compression method nobody implements) or UnicodeDecodeError (a member
# name that is no UTF-8). So that arm quotes what python3 raised and says that this helper
# cannot tell from it whether the archive or this run is what failed, and a full disk part way
# through the write and a tampered archive are refused in the one sentence that rules out
# neither. The tar arm names the member, the archive and tar's exit, and leaves tar's own
# words on stderr to say why. Nothing unverified is installed in any of them: this returns
# non-zero and the caller places nothing.
# It is public because .github/scripts/ci-tools-proof.sh takes a row's pinned member out of the
# archive ci_tools_fetch_primary staged for it, rather than fetching that archive a second time
# through ci_tools_fetch, and a second extractor there would be a second rule for how an
# archive is read.
ci_tools_extract() {
  local archive=$1 member=$2 destination=$3 url=$4 name reported='' status=0
  # The one failure of this run either arm can prove, and the one thing proved before a byte
  # of the archive is read: a destination this run cannot even open. It guards both arms,
  # because neither reader may call the archive's bytes into question for a file this runner
  # would not let it write. bash says on stderr why it could not be opened.
  if ! : > "$destination"; then
    printf 'this run cannot write %s, so %s was never read for %s, and nothing here is a statement about the bytes that address served\n' \
      "$destination" "$url" "$member"
    return 1
  fi
  # The suffix is read off the address with no query and no fragment, which PyPI writes.
  name=${url%%\?*}
  name=${name%%#*}
  case "$name" in
    *.whl | *.zip)
      # python3's stderr is read rather than discarded: for anything but an absent member it
      # is the only thing that says what was wrong, and the two sentences below quote its last
      # line, which is the exception and its message.
      reported=$(
        PYTHONDONTWRITEBYTECODE=1 python3 -c '
import shutil
import sys
import traceback
import zipfile
import zlib

archive, member, destination, absent, damaged = sys.argv[1:6]
try:
    with zipfile.ZipFile(archive) as held, held.open(member) as source, open(destination, "wb") as sink:
        shutil.copyfileobj(source, sink)
except KeyError:
    # The archive read, and it holds no member of that name.
    sys.exit(int(absent))
except (zipfile.BadZipFile, zlib.error, EOFError):
    # What the archive itself raises, which is the one group of failures that is a signal
    # about these bytes. The traceback is printed here rather than left to python3, because
    # this exit is a code the shell above chose and not one of python3 itself, and that shell
    # quotes the last line of this report all the same.
    traceback.print_exc()
    sys.exit(int(damaged))
# Every other exception is left to python3 itself, so its traceback reaches stderr and its own
# exit 1 reaches the caller: it may be the archive and it may be this run — an OSError is
# raised by the stream of a damaged member and by the destination alike — so the sentence it
# gets clears neither. This program is a single-quoted argument, so no line of it may carry an
# apostrophe.
' "$archive" "$member" "$destination" "$ci_tools__no_such_member" "$ci_tools__damaged_archive" 2>&1
      ) || status=$?
      # Whatever went wrong, what the reader had already written is taken away, so no caller
      # finds a part of a member at the destination it named.
      [ "$status" -eq 0 ] || rm -f -- "$destination"
      if [ "$status" -eq "$ci_tools__no_such_member" ]; then
        printf 'the archive at %s holds no %s (python3 exit %s)\n' "$url" "$member" "$status"
        return 1
      fi
      # The whole of what python3 said goes to the log, frames and all, and the sentence
      # carries its last line, which is the exception itself: a refusal is one annotation
      # line, and a reader who wants the rest reads the lines above it.
      if [ "$status" -eq "$ci_tools__damaged_archive" ]; then
        [ -z "$reported" ] || printf '%s\n' "$reported" >&2
        printf 'the archive at %s did not give up %s, and not because it holds no such member: %s. What that address served is no readable archive of the bytes this row pins, which is a signal about those bytes (python3 exit %s)\n' \
          "$url" "$member" "${reported##*$'\n'}" "$status"
        return 1
      fi
      if [ "$status" -ne 0 ]; then
        [ -z "$reported" ] || printf '%s\n' "$reported" >&2
        printf 'the archive at %s did not give up %s: %s. python3 raised that, and this helper cannot tell from it whether the archive or this run is what failed — damage reaches python3 as an OSError, a RuntimeError, a NotImplementedError or a UnicodeDecodeError, and so does a destination that filled up part way through the write — so these bytes are not cleared (python3 exit %s)\n' \
          "$url" "$member" "${reported##*$'\n'}" "$status"
        return 1
      fi
      ;;
    *)
      tar -xzOf "$archive" "$member" > "$destination" || status=$?
      if [ "$status" -ne 0 ]; then
        rm -f -- "$destination"
        # No set of causes is named, because this arm knows none: an absent member, a damaged
        # stream and a destination that filled up part way through the write are one exit 2
        # here. What tar printed is the only thing that says more, and it is on stderr above.
        printf 'the archive at %s did not give up %s (tar exit %s); what tar printed on stderr above says why\n' \
          "$url" "$member" "$status"
        return 1
      fi
      ;;
  esac
  return 0
}

# ci_tools__fetch_address <row> <side> <max_time> <file> <seam>: the bytes one URL of the row
# serves, into <file>, with that URL put through <seam> first. It is how every source this file
# reads at an address of its own is fetched, and the word decides only which seam that is:
# ci_tools__fetch_release below passes ci_tools_github_url and ci_tools__fetch_url passes
# ci_tools__seamed_url.
#
# The side decides which fields are read. `second` reads `second_url` and takes `second_member`
# out of what arrived, because the bytes the row's own `digest` answers for are that member.
# `primary` reads `primary` and takes nothing out: the archive is held to the row's
# `primary_digest` first, and ci_tools__primary_open does that and then takes `primary_member`
# out, so one place takes the primary's member out whatever word the primary is written in.
#
# Prints what happened, and returns non-zero, where the service did not serve the bytes, and
# returns the write error itself where this run is what could not write the file curl was
# given.
ci_tools__fetch_address() {
  local row=$1 side=$2 max_time=$3 file=$4 seam=$5
  local url address member='' archive='' target=$file report status=0
  if [ "$side" = primary ]; then
    ci_tools__read url "$row" primary || return 1
  else
    ci_tools__read url "$row" second_url || return 1
    ci_tools__read member "$row" second_member || return 1
  fi
  if [ -n "$member" ]; then
    archive=$(ci_tools__tempfile "$row-archive") || {
      printf 'this run could not make a temporary file for the archive at %s\n' "$url"
      return 1
    }
    target=$archive
  fi
  address=$("$seam" "$url")
  ci_tools_curl "$target" "$max_time" "$address" || status=$?
  if [ "$status" -ne 0 ]; then
    [ -z "$archive" ] || rm -f -- "$archive"
    # The word of the source stands before this sentence wherever it is printed, so what is
    # left to say is what curl did, which is what the primary's line has said since this file
    # read one source. The one exit that says more is the write error, and ci_tools__curl_report
    # is where this file says it.
    ci_tools__curl_report "$status" "$target" "curl exit $status"
    return
  fi
  [ -n "$member" ] || return 0
  report=$(ci_tools_extract "$archive" "$member" "$file" "$url") || status=$?
  rm -f -- "$archive"
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$report"
    return 1
  fi
  return 0
}

# ci_tools__fetch_release <row> <side> <max_time> <file>: the `github-release` word on either
# side, which is a release asset read through the CI_TOOLS_GITHUB_BASE seam.
ci_tools__fetch_release() {
  ci_tools__fetch_address "$1" "$2" "$3" "$4" ci_tools_github_url
}

# ci_tools__fetch_url <row> <side> <max_time> <file>: the `pypi` and `pulumi-releases` words on
# either side, which is the whole address the row writes, read through the CI_TOOLS_URL_BASE
# seam.
ci_tools__fetch_url() {
  ci_tools__fetch_address "$1" "$2" "$3" "$4" ci_tools__seamed_url
}

# ci_tools__source_fetch <row> <primary|second> <source word> <variable>: the function that
# reads that word on that side, into <variable>, and a refusal naming the row, the field it was
# written in and the words that side is written in where this file reads none.
# It is the one list of the words a source is written in, and it is read for both sides:
# ci_tools_fetch and ci_tools__second_source fetch a row's bytes with the function it answers,
# and ci_tools_source_name above prints a row's word only where it answers one, so a word the
# manifest carries and no source of this file reads is refused by both, in one sentence.
# `ghcr-mirror` and `npm` answer on the second side alone, which is what makes a row whose
# `primary_source` is one of them a refusal that names the side it was written on.
#
# THE PLACES OF THIS TREE THAT DECIDE A SOURCE WORD are written here and counted nowhere else,
# so adding a source is these four edits and no hunt for a fifth. Each of the other three says
# that this comment holds the list and keeps no list of its own.
#
#   1. This case, which says which function reads a word on which side, and so which words a
#      row can be fetched by and named by at all.
#   2. The SECOND_FIELDS, PRIMARY_SOURCES and URL_SOURCES tables of
#      .github/scripts/check-ci-tools.sh, which decide which words a row may write on each
#      side and which fields each word then makes that row carry, so a row whose word this
#      file would never be asked about is refused by the gate.
#   3. `route_of` in .github/scripts/ci-tools-proof.sh, which says which route of that rig's
#      origin a word is served on.
#   4. The GITHUB_WORDS and URL_WORDS sets of that rig's origin program, which is a program of
#      its own started with the manifest and no word of that shell, and which decide which of
#      a row's two addresses each of those two routes serves a name from.
ci_tools__source_fetch() {
  local row=$1 side=$2 word=$3 __variable=$4
  case "$side/$word" in
    */github-release) printf -v "$__variable" '%s' ci_tools__fetch_release ;;
    */pypi | */pulumi-releases) printf -v "$__variable" '%s' ci_tools__fetch_url ;;
    second/ghcr-mirror) printf -v "$__variable" '%s' ci_tools__fetch_ghcr ;;
    second/npm) printf -v "$__variable" '%s' ci_tools__fetch_npm ;;
    primary/*)
      ci_tools__error "$row: the manifest's primary_source is '$word', and this file reads github-release, pypi and pulumi-releases as a primary"
      return 1
      ;;
    *)
      ci_tools__error "$row: the manifest's second_source is '$word', and this file reads github-release, ghcr-mirror, npm, pypi and pulumi-releases"
      return 1
      ;;
  esac
}

# ci_tools__second_source <row> <dest> <the primary's word> <what the primary did>: the second
# source alone. The last two arguments are empty where there was no primary to try, and the
# refusal then names one source.
# A write error of this run is refused here in the words ci_tools_fetch refuses it in, and that
# refusal names no source at all.
ci_tools__second_source() {
  local row=$1 dest=$2 primary_word=$3 primary_report=$4
  local second source fetch max_time file report status=0
  ci_tools__read second "$row" second_source || return 1
  ci_tools__read max_time "$row" max_time || return 1
  # The row's own word is the source a line and a refusal name, so nothing here translates it,
  # and the list above decides the one thing left: which function reads that source on this
  # side. A word the manifest carries and no fetch answers to is refused there rather than left
  # to fetch nothing, since an empty temporary file would be refused by the digest check
  # instead, which is the wrong refusal for the wrong reason.
  source=$second
  ci_tools__source_fetch "$row" second "$second" fetch || return 1
  file=$(ci_tools__tempfile "$row") || {
    ci_tools__error "$row: this run could not make a temporary file to fetch into"
    return 1
  }
  report=$("$fetch" "$row" second "$max_time" "$file") || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$file"
    [ -n "$report" ] || report="the fetch failed (exit $status)"
    if [ "$status" -eq "$ci_tools__write_error" ]; then
      # This run is what failed, so the refusal names no source. A primary that was read and
      # did not serve has its own line above this one.
      ci_tools__error "$row: $report — refusing"
      return 1
    fi
    if [ -n "$primary_word" ]; then
      ci_tools__error "$row: no source served the bytes; $primary_word: $primary_report; $source: $report"
    else
      ci_tools__error "$row: no source served the bytes; $source: $report"
    fi
    return 1
  fi
  ci_tools__place "$row" "$file" "$dest" "$source"
}

# ci_tools__primary_digest <row> <digest variable> <prose variable>: the digest the bytes the
# primary serves are held to, and the prose saying what it is of, into those two variables.
# A row whose `primary_digest` is not empty pins the archive there, because its own `digest` is
# of something inside that archive: uv's `digest` is of the binary, which GitHub's tarball and
# PyPI's wheel each hold, so the bytes GitHub serves have a digest of their own and that field
# is it. A row that leaves it empty is served exactly the bytes it pins, so the digest of what
# the primary serves is the row's own `digest` and the prose is empty.
# It is the one statement of that rule, and ci_tools_fetch_primary is its one caller: that is
# the fetch whose destination holds the archive itself, so it is the one place the fallback to
# the row's own `digest` is needed. ci_tools__primary_open below holds the archive to the
# `primary_digest` a row names and to nothing where a row names none, since a row that names
# none has nothing to take out either.
ci_tools__primary_digest() {
  local __row=$1 __digest=$2 __of=$3 __named
  ci_tools__read __named "$__row" primary_digest || return 1
  if [ -n "$__named" ]; then
    printf -v "$__digest" '%s' "$__named"
    ci_tools__read "$__of" "$__row" primary_digest_of || return 1
    return 0
  fi
  printf -v "$__of" '%s' ''
  ci_tools__read "$__digest" "$__row" digest
}

# ci_tools__primary_open <row> <file> <url> <the primary's word>: what stands between the bytes
# the primary served and the digest check of the row, for a row that names one or both of two
# fields. <url> is the address those bytes came from: it decides how an archive is read, and it
# is what a refusal names, together with the word that source is written in. A row that names
# neither field leaves <file> exactly as it arrived.
#
# `primary_digest` is the sha256 of the bytes the primary serves, and the archive is held to it
# here, before anything is taken out of it. A mismatch is the supply-chain signal a mismatch on
# the row's own `digest` is, so the caller answers for the bytes that arrived and reads no
# second source after one.
#
# `primary_member` is the member of that archive the row's own `digest` is of, and it replaces
# the archive at <file>, so what ci_tools__place then verifies and moves is the member and the
# archive reaches no destination. That replacement is what the name says: this opens the
# archive the primary served and leaves the member in its place.
ci_tools__primary_open() {
  local row=$1 file=$2 url=$3 word=$4
  local digest of='' member extracted report status=0
  ci_tools__read digest "$row" primary_digest || return 1
  if [ -n "$digest" ]; then
    ci_tools__read of "$row" primary_digest_of || return 1
    ci_tools__hold "$row" "$file" "$word" "$digest" "$of" || return 1
  fi
  ci_tools__read member "$row" primary_member || return 1
  [ -n "$member" ] || return 0
  extracted=$(ci_tools__tempfile "$row-member") || {
    ci_tools__error "$row: this run could not make a temporary file for the $member it must take out"
    return 1
  }
  report=$(ci_tools_extract "$file" "$member" "$extracted" "$url") || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$extracted"
    ci_tools__error "$row: $word served the bytes, and $report — refusing"
    return 1
  fi
  if ! mv -f -- "$extracted" "$file"; then
    rm -f -- "$extracted"
    ci_tools__error "$row: $word served the bytes, and this run could not keep the $member it took out of them — refusing"
    return 1
  fi
  return 0
}

# ci_tools_fetch <row> <dest>: the row's bytes at <dest>, from the primary where it serves them
# and from the second source where it does not.
# OF THE THREE CAUSES THIS FILE'S HEADER NAMES, THIS FETCH PROVES ONE: a local write error of
# this run, which curl answers with exit 23. It stops this fetch where it stands, on either
# side, and no second source is read, because no other address cures a file this runner will
# not let a fetch write.
# Every other failure of the primary IS a reason to read the second source, which is what a
# 504, a reset and a timeout are, and the line above that second fetch says so. The two
# failures that stop this fetch where the bytes did arrive — a digest mismatch, and damage the
# archive itself raised — stop it for the opposite reason: those bytes answer for themselves.
ci_tools_fetch() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_fetch takes a row and a destination, and this call gave $#"
    return 1
  fi
  local row=$1 dest=$2 primary word fetch max_time file report status=0
  ci_tools__read primary "$row" primary || return 1
  if [ -z "$primary" ]; then
    # A third party fetches this row's primary bytes, so there is nothing here to try first.
    ci_tools__second_source "$row" "$dest" '' ''
    return
  fi
  ci_tools__read word "$row" primary_source || return 1
  ci_tools__read max_time "$row" max_time || return 1
  # Which source reads that URL is the row's own `primary_source`, and the list answers it
  # before a byte is fetched, so a word no primary is read with is refused here rather than
  # after an outage has sent this row to its second source.
  ci_tools__source_fetch "$row" primary "$word" fetch || return 1
  file=$(ci_tools__tempfile "$row") || {
    ci_tools__error "$row: this run could not make a temporary file to fetch into"
    return 1
  }
  report=$("$fetch" "$row" primary "$max_time" "$file") || status=$?
  if [ "$status" -eq 0 ]; then
    # The bytes arrived. Whatever they are, this call answers for them: a mismatch here is a
    # supply-chain signal, and the second source is never read after one, which holds for the
    # row's `primary_digest` as much as for its own `digest`.
    if ! ci_tools__primary_open "$row" "$file" "$primary" "$word"; then
      rm -f -- "$file"
      return 1
    fi
    ci_tools__place "$row" "$file" "$dest" "$word"
    return
  fi
  rm -f -- "$file"
  [ -n "$report" ] || report="the fetch failed (exit $status)"
  if [ "$status" -eq "$ci_tools__write_error" ]; then
    # This run is what failed, so the refusal names no service, and this fetch stops here.
    ci_tools__error "$row: $report — refusing"
    return 1
  fi
  printf 'ci-tools: %s: %s did not serve the bytes (%s); trying the second source\n' \
    "$row" "$word" "$report"
  ci_tools__second_source "$row" "$dest" "$word" "$report"
}

# ci_tools_fetch_primary <row> <dest>: the row's primary bytes at <dest>, as the primary serves
# them: the archive and not the member, held to the row's `primary_digest` where it names one
# and to its own `digest` where it does not, and fetched through whichever word the row's
# `primary_source` names. It reads no second source, because the question it answers is what
# that one URL served.
# .github/scripts/ci-tools-proof.sh stages a row's primary bytes with it: a row whose `digest`
# is of a member needs the archive the primary actually serves, which is not the file
# ci_tools_fetch leaves at a destination.
ci_tools_fetch_primary() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_fetch_primary takes a row and a destination, and this call gave $#"
    return 1
  fi
  local row=$1 dest=$2 primary word fetch max_time digest of file report status=0
  ci_tools__read primary "$row" primary || return 1
  if [ -z "$primary" ]; then
    ci_tools__error "$row: a third party fetches this row's primary bytes, so this file has no primary URL to read them from — refusing"
    return 1
  fi
  ci_tools__read word "$row" primary_source || return 1
  ci_tools__read max_time "$row" max_time || return 1
  ci_tools__source_fetch "$row" primary "$word" fetch || return 1
  ci_tools__primary_digest "$row" digest of || return 1
  file=$(ci_tools__tempfile "$row") || {
    ci_tools__error "$row: this run could not make a temporary file to fetch into"
    return 1
  }
  report=$("$fetch" "$row" primary "$max_time" "$file") || status=$?
  if [ "$status" -ne 0 ]; then
    rm -f -- "$file"
    [ -n "$report" ] || report="the fetch failed (exit $status)"
    if [ "$status" -eq "$ci_tools__write_error" ]; then
      ci_tools__error "$row: $report — refusing"
      return 1
    fi
    ci_tools__error "$row: $word did not serve the bytes ($report) — refusing"
    return 1
  fi
  ci_tools__place "$row" "$file" "$dest" "$word" "$digest" "$of"
}

# ci_tools_fetch_fallback <row> <dest>: the second source alone, for the rows whose primary bytes a
# third party fetches. .github/actions/setup-bun and .github/actions/install-jj call it when their
# own install did not land the file.
ci_tools_fetch_fallback() {
  if [ "$#" -ne 2 ]; then
    ci_tools__error "ci_tools_fetch_fallback takes a row and a destination, and this call gave $#"
    return 1
  fi
  ci_tools__second_source "$1" "$2" '' ''
}
