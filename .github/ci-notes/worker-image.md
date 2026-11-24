# Notes for the worker image's build

The derivations of `.github/workflows/worker-image.yaml`, and of the bounds of the downloads of
`packages/daemon/docker/worker.Dockerfile`, which point into this file. Each `# why:` line names
the section below that holds the derivation its value rests on. A value's own arithmetic stays
beside the value.

## The bound of each download

Every curl download of the Dockerfile goes through `.github/scripts/fetch.sh`, which cuts an
attempt 30 s into a stall (under 10,000 bytes a second, or no connection) and at the bound the
call passes. The bound holds the download while it keeps moving at the slowest rate CI has seen:
in job 137969866080 (run 45253427255) PyPI served a 17,211,825-byte wheel at about 257 kB/s, so
each bound is the file's size at 250,000 bytes a second, rounded up to the minute, and at least
60 s. The sizes are the `Content-Length` each address answered a `HEAD` with on 2026-11-24, at the
pins of that day:

| Download (stage) | Bytes | At 250,000 B/s | Bound | Worst case, 3 × bound + 3 s |
| --- | ---: | ---: | ---: | ---: |
| mise (tools) | 37,439,195 | 150 s | 180 s | 543 s |
| Oh My Pi (tools) | 172,730,371 | 691 s | 720 s | 2,163 s |
| jj (tools) | 10,171,787 | 41 s | 60 s | 183 s |
| gh (tools) | 14,863,663 | 59 s | 60 s | 183 s |
| gh-stack (tools) | 26,885,107 | 108 s | 120 s | 363 s |
| uv (toolchain) | 16,682,639 | 67 s | 120 s | 363 s |
| Node (toolchain) | 32,026,916 | 128 s | 180 s | 543 s |
| AWS CLI (toolchain) | 74,130,102 | 297 s | 300 s | 903 s |
| Go (toolchain) | 70,588,678 | 282 s | 300 s | 903 s |
| kubectl (toolchain) | 59,121,826 | 236 s | 240 s | 723 s |
| yq (toolchain) | 14,168,224 | 57 s | 60 s | 183 s |
| Chromium (toolchain) | 206,050,610 | 824 s | 840 s | 2,523 s |
| Google Cloud SDK (gcloud) | 94,824,123 | 379 s | 420 s | 1,263 s |
| PostgreSQL key (runtime-base) | 4,812 | 0 s | 60 s | 183 s |
| 17 licence files and 7 `HEAD` checks (notices) | a few kB each | 0 s | 60 s | 183 s each |

A fetch fails by name inside three bounds and curl's two pauses, 3 × bound + 3 s (fetch.sh's header
says why). Until 2026-11-24 every bound was 60 s, twice the slowest green fetch rounded up to the
minute, the rule network-install.sh's kinds follow; that cut Chromium's zip below about 3.4 MB/s
while it was still moving, where main set no clock on it. The green fetches are far under the size
rule. They were read off BuildKit's own clock (the seconds since a RUN started that it prints
before each line of the RUN's output) in the 27 green builds of 2026-11-23: the 25 newest green
worker_image builds of release.yaml and the two pull-request builds of this change, jobs
137748816723 and 137784493728. Each RUN prints a line right after its downloads end, so:

- mise (tools stage): `mise.tar.gz: OK`, at most 0.98 s, job 137826539543 (run 45212669243).
- Oh My Pi, jj, gh and gh-stack (tools stage): the `tools: <tool>: <url>, sha256 ...` line each
  fetch prints after its check, less the line before it: Oh My Pi at most 3.10 s, job
  137784493728 (run 45201056167); jj 0.49 s, job 137826539543; gh 0.54 s, job 137784493728;
  gh-stack 0.66 s, job 137787948171 (run 45197933390), in the 6 builds that had it.
- uv, Node, the AWS CLI, Go, kubectl, yq and Chromium (toolchain stage): the first `: OK` of the
  sha256sum after all seven, at most 6.69 s for the seven together, job 137646499135 (run
  45158739984).
- The Google Cloud SDK: `sdk.tar.gz: OK`, at most 2.29 s, job 137749053475 (run 45190750789).
- The seventeen license files and seven checks of the notices stage print nothing until the RUN
  ends: the whole RUN took at most 8.1 s, job 137787948171.
- The PostgreSQL key (runtime-base stage) is fetched in the RUN that installs the image's Debian
  packages, whose output does not set it apart; the browser image fetches the same key from the
  same address, at most 0.71 s (`.github/ci-notes/browser-image.md`).

A version bump reads its file's size again and moves the bound, and the limit below, when the size
passes what the bound holds at that rate: gh's archive is 59 s of its 60 s.

The go-licenses fetch runs through `.github/scripts/network-install.sh`'s bound for go: 425 s.

## The build job's limit, held to the build's downloads

BuildKit builds the stages beside each other, and a RUN waits only for the RUNs before it in its
own stage and for the stages it copies or mounts from. That is measured in the builds above, read
from each RUN's `DONE <s>s` line and the time the log took it: in job 137784493728 (run
45201056167) the tools stage's archive RUN ran 10.4 s, every second of it inside the toolchain
RUN's 30.0 s (both from about 15:05:32Z), and the notices stage's fetch RUN ran its 6.2 s inside
the Google Cloud SDK RUN; job 137748816723 (run 45190825753) shows the same two overlaps, 6.9 s and
3.7 s. So the build's worst case is its longest chain of downloads, not their sum, each download at
its worst case in the table above:

- tools: mise and the four archives, 543 s + 2,163 s + 183 s + 183 s + 363 s = 3,435 s, and then
  the notices stage, built on the tools stage: 24 × 183 s = 4,392 s. 7,827 s.
- toolchain: its seven archives, 363 s + 543 s + 903 s + 903 s + 723 s + 183 s + 2,523 s =
  6,141 s, then the Google Cloud SDK, 1,263 s. 7,404 s.
- go-licenses, 425 s; runtime-base, 183 s.

The rest of the build is measured. The "Build and push" step took 660 s at its slowest of those 27
builds, job 137913895923 (release.yaml run 45236058230), and that time holds every install the
build makes outside these two scripts (apt-get, bun install, go install, go mod download, npm ci
and uv), which have no bound of their own but this limit. The job's other steps took 180 s in that
same job, 12 s of them the builder's setup, whose pull of its BuildKit image is counted as the
runner's own pulls are, at 240 s (`.github/ci-notes/ci.md`, "The runner's own pulls of a job's
service and container images"): 180 s - 12 s + 240 s = 408 s.

So the limit is 660 s + 7,827 s + 408 s = 8,895 s, rounded up to 149 minutes. The 30 minutes main
gives the job would end a build whose downloads stalled in GitHub's cancellation, which names
nothing, before fetch.sh failed one by name.
