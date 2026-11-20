# shellcheck shell=bash
# Sourced by autodeploy_test.sh and up-dispatch_test.sh: Docker Compose 2.12.2, the oldest release
# the deploy scripts are held to. The binary comes out of this repository's own copy of
# docker:20.10.20-cli (.github/ci-images.yaml), pulled by digest, so no run downloads it from
# GitHub: that image's Compose plugin is the v2.12.2 release asset byte for byte, on amd64 and on
# arm64. .github/scripts/check-ci-images.sh reads this file and holds the reference to that row.
compose_2_12_image=ghcr.io/cybermonklabs/legion/ci@sha256:466edff9fbc8e1326259d04412f55684c2e0f79c574eb9782c252011a87d8046

# take_compose_2_12 DOCKER DEST copies the Compose plugin out of that image into DEST with DOCKER,
# the real docker CLI, and fails, naming the step, where it cannot or where DEST does not answer
# 2.12.2. The container is created and never started, and it is removed by its own ID.
take_compose_2_12() {
  local docker=$1 dest=$2 container failed=
  container=$("$docker" create "$compose_2_12_image") || {
    printf 'could not create a container of %s\n' "$compose_2_12_image" >&2
    return 1
  }
  "$docker" cp "$container:/usr/libexec/docker/cli-plugins/docker-compose" "$dest" ||
    failed="could not copy Compose out of $compose_2_12_image"
  "$docker" rm "$container" >/dev/null ||
    failed="${failed:+$failed; }could not remove the container $container of $compose_2_12_image"
  if [[ -n "$failed" ]]; then
    printf '%s\n' "$failed" >&2
    return 1
  fi
  if [[ "$("$dest" version --short)" != 2.12.2 ]]; then
    printf 'the Compose in %s does not answer 2.12.2\n' "$compose_2_12_image" >&2
    return 1
  fi
}
