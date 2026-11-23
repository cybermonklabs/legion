#!/usr/bin/env bash
# The Oh My Pi build .omp-pin names is a release of a fork, cut by hand, and a release's tag can be
# moved: mise installs whatever bytes the tag names on the day it runs. So each install of the pin
# is checked here, against .omp-pin.sha256, before anything runs it. The check covers two things
# and nothing more: the bytes of the one file the digest file names (bin/omp), and that the install
# directory holds nothing else at any depth: no other file, no directory but those the named file
# sits in (bin), and no symbolic link, the named file included, which must be a regular file.
#
# The second is as much the point as the first. The worker image copies the whole install directory
# to /opt/omp, and /opt/omp/bin stands on every role's PATH ahead of /usr/local/bin and /usr/bin, so
# a release that kept bin/omp byte for byte and added bin/kubectl beside it would put an unchecked
# kubectl in front of the real one for every role. A real install holds bin and bin/omp alone: mise
# (measured with v2026.8.12) keeps its own files, .mise.backend.toml and the version links, in the
# directory above it.
#
# The digest file is one line in sha256sum's format: 64 lower-case hex digits, two spaces, and the
# path of the file relative to the install directory (`mise where "$pin"`). A pin bump writes the new
# build's line in the same commit, hashed from the build the bump was tested on:
#   (cd "$(mise where "$pin")" && sha256sum bin/omp) > .omp-pin.sha256
#
# This script lists the install directory, following no link, and hashes the named file; it runs
# nothing it checks. On a match it prints one line naming the file and its digest and exits 0. It
# exits 1 with a message on stderr, refusing at the first of these it meets: a digest file not in
# that format, or whose path leaves the install directory; an install directory that is missing or
# is a symbolic link; any entry the directory holds besides the named file and the directories it
# sits in, each such entry named by its path relative to the install directory, with what it is; a
# missing file, in one line naming the file and the digest it expected; another digest, in one line
# naming the file, the digest it expected and the digest it read.
#
# Run from anywhere: .github/scripts/omp-digest.sh <install dir> <digest file>
# Its callers are .github/actions/install-omp and the tools stage of
# packages/daemon/docker/worker.Dockerfile, each between the install and the first run of the binary.
set -euo pipefail

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <install dir> <digest file>" >&2
  exit 2
fi
dir=$1 digests=$2

lines=()
if ! { mapfile -t lines <"$digests"; } 2>/dev/null; then
  echo "$digests: cannot be read" >&2
  exit 1
fi
# An explicit digit list rather than a range: under a UTF-8 locale bash's bracket ranges can match
# characters past ASCII.
hex='[0123456789abcdef]'
pattern="^($hex{64})  ([^/[:space:]][^[:space:]]*)\$"
if [ "${#lines[@]}" -ne 1 ] || ! [[ ${lines[0]} =~ $pattern ]]; then
  echo "$digests: not one line of 64 lower-case hex digits, two spaces and a relative path" >&2
  exit 1
fi
want=${BASH_REMATCH[1]} path=${BASH_REMATCH[2]}
case "/$path/" in
*/../*)
  echo "$digests: the path $path leaves the install directory" >&2
  exit 1
  ;;
esac

if [ ! -d "$dir" ] || [ -L "$dir" ]; then
  echo "$dir: not a directory itself (missing, another kind of file, or a symbolic link)" >&2
  exit 1
fi
# Every entry under the install directory, at any depth, as find's type letter for it (d a
# directory, f a regular file, l a symbolic link, another letter any other kind; find's default, -P,
# follows no link) and its path relative to the directory, sorted by path so a refusal names them
# the same way every time. Read through a process substitution; wait gives the status of find and
# sort (pipefail holds in the substitution too). The entries allowed are the named file, as a
# regular file, and each directory it sits in; any other is refused, after the walk names them all.
unnamed=()
while IFS= read -r -d '' entry; do
  kind=${entry%% *} rel=${entry#* }
  case $kind in
  f) [ "$rel" != "$path" ] || continue; what='a regular file' ;;
  d) [[ $path != "$rel"/* ]] || continue; what='a directory' ;;
  l) what='a symbolic link' ;;
  *) what="a file of find type $kind" ;;
  esac
  printf -v rel '%q' "$rel"
  unnamed+=("$rel: $what")
done < <(find "$dir" -mindepth 1 -printf '%y %P\0' | LC_ALL=C sort -z -t ' ' -k 2)
if ! wait "$!"; then
  echo "$dir: cannot be listed" >&2
  exit 1
fi
if [ "${#unnamed[@]}" -gt 0 ]; then
  echo "$dir: holds more than $path, the regular file $digests names, and the directories it is in:" >&2
  printf '  %s\n' "${unnamed[@]}" >&2
  exit 1
fi

file=$dir/$path
if [ ! -f "$file" ]; then
  echo "$file: expected sha256 $want ($digests), read nothing: no such file" >&2
  exit 1
fi
got=$(sha256sum <"$file")
got=${got%% *}
if [ "$got" != "$want" ]; then
  echo "$file: expected sha256 $want ($digests), read sha256 $got" >&2
  exit 1
fi
echo "$file: sha256 $got, as $digests pins"
