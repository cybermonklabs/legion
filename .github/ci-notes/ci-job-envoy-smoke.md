# Notes for the envoy-smoke job's steps

The derivations of `.github/workflows/ci-job-envoy-smoke.yaml`, which points into this file. Each
`# why:` line of that workflow names the section below that holds the derivation its value rests
on. A value's own arithmetic stays beside the value.

Every wait on the network this job makes runs through `.github/scripts/network-install.sh`, in its
steps and in the RUN instructions of the images it builds (`packages/envoy/docker/Dockerfile`
bind-mounts the script for each RUN that installs). Each such install can take three attempts cut at
its kind's bound, each killed 15 s after its SIGTERM, with two 10 s pauses, before it fails by name:
425 s for a docker pull, a bun install or a go fetch (120 s bounds), 245 s for an apt command (60 s).
A limit that ends before that ends a stalled install in GitHub's cancellation, which names nothing,
and rule 22 of `.github/scripts/check-ci-filters.sh` refuses it.

## The job's limit, held to the installs it runs

The slowest green run of this job read, of 293 from 2026-11-16 to 2026-11-20, took 844 s, job
136002249440. The pull step pulls four images before the builds and the tests read them
(testnats.HubImage and the three FROM images of `packages/envoy/docker/Dockerfile`), 425 s each. The
listener image's build can take 1,765 s for its installs (the section below). So the limit is
844 s + 4 × 425 s + 1,765 s = 4,309 s, rounded up to 72 minutes.

## The image build's limit

The listener image's build step took 259 s at its slowest of 1,294 green runs from 2026-11-03 to
2026-11-20, job 127958676432. Its RUN instructions install through network-install.sh: one bun
install (425 s), two go fetches (`go-third-party-notices.sh --install` and `go mod download`, 425 s
each) and two apt commands (245 s each), 1,765 s in all. Its FROM images are on the runner already,
pulled by the step before, so the builder pulls none of them. So the step's limit is 259 s +
1,765 s = 2,024 s, rounded up to 34 minutes. The held-port image's build needs no limit of its own
for this: its FROM images are the listener image this job just built and an image the pull step
pulled, and its RUN instructions install nothing.
