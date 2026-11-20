#!/usr/bin/env bash
# runner-tmux.sh: puts a tmux on PATH for the job that runs it, the runner image's own, reading no
# network. ci.yaml's daemon-go and pi-envoy jobs run it in their step "Use the runner image's
# tmux": daemon-go's tmux runtime tests and the Stage 1 proof's boot, and pi-envoy's
# extensions/tmux-place-omp.test.ts, need a real tmux.
#
# The ubuntu-24.04 runner image ships tmux: `apt-get install tmux` answered `tmux is already the
# newest version (3.4-1ubuntu0.2)` in job 136050670942 of run 44664541053. The step this replaces
# ran `sudo apt-get update && sudo apt-get install -y --no-install-recommends tmux` first, which
# read 13.3 MB of package lists to install nothing, and in run 44665181416 it waited on a mirror
# from 03:05:59Z until daemon-go's 70-minute limit cancelled it. An image without tmux gets a
# warning that the image changed, and Ubuntu's package through network-install.sh, which bounds,
# retries and names each of the two apt commands that wait on the network: the update, and the
# package's download (--download-only). The install from that download runs unwrapped
# (--no-download), so no bound can end it while dpkg writes. The update carries --error-on=any:
# without it, apt-get update exits 0 on a mirror it could not read, and the install after it then
# fails on a package no list holds, which reads as the change's own fault rather than the network's.
#
# Run from anywhere: .github/scripts/runner-tmux.sh
set -euo pipefail

if command -v tmux > /dev/null; then
  tmux -V
  exit 0
fi
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
echo "::warning::this runner image holds no tmux, so this job installs Ubuntu's package from the network"
"$root/.github/scripts/network-install.sh" apt -- sudo apt-get update --error-on=any
"$root/.github/scripts/network-install.sh" apt -- sudo apt-get install -y --no-install-recommends --download-only tmux
sudo apt-get install -y --no-install-recommends --no-download tmux
tmux -V
