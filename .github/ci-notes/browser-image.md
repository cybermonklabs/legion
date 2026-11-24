# Notes for the browser image's build

The derivations of `.github/workflows/browser-image.yaml` and of the bounds of
`.github/docker/browser.Dockerfile`, which point into this file. Each `# why:` line names the
section below that holds the derivation its value rests on. A value's own arithmetic stays beside
the value.

## The reads of the network in the image's install RUN

The install RUN of `.github/docker/browser.Dockerfile` reads the network three times, and each is
bounded so a host that answers and then all but stops fails the build by name rather than holding
the job until its limit. The three green builds of the recipe's last day were read for each read's
time: jobs 128730667606, 128991520632 and 129036051255 (runs 42480325123, 42565616054 and
42579935203, 2026-11-08 and 2026-11-09, the last of which built the image the row pins,
sha256:69c3a316).

- The PostgreSQL signing key, read by `.github/scripts/fetch.sh` from www.postgresql.org: 0.54 s,
  0.71 s and 0.44 s. fetch.sh cuts an attempt 30 s into a stall and at the bound its call passes,
  the file's size at 250,000 bytes a second rounded up to the minute, and at least 60 s (fetch.sh's
  header says why): the key's 4,812 bytes take under a second at that rate, so 60 s, which is also
  twice the slowest, 1.42 s, rounded up to the minute. It makes up to three attempts, with curl's
  own pauses of 1 s and 2 s, and starts none after 120 s: 183 s at most before it fails the RUN by
  name (fetch.sh's header says the form).
- `apt-get update --error-on=any`: 2.2 s, 2.2 s and 1.3 s (35.2 MB of lists). It runs through
  network-install.sh, under its bound for apt, 60 s an attempt, which that kind's measurement sets
  and these fit inside: 245 s before it fails by name.
- The packages' download, `apt-get install --download-only`: 5.2 s, 7.2 s and 1.5 s (88 MB, 68
  packages). Under the same bound, 245 s. The install from what was downloaded, `--no-download`,
  reads no network and runs unwrapped.

## The build step's limit

The build step took 65.7 s at its slowest of the three, job 128991520632, and 10.15 s of it were
the three reads above (0.71 s + 2.22 s + 7.22 s). The rest, 55.55 s, with the 15 per cent the
budget rule of this workflow gives a measured step, is 63.9 s; the three reads at their worst are
183 s + 245 s + 245 s. So the step's limit is 63.9 s + 673 s = 736.9 s, rounded up to 13 minutes.
The pull of the base image the build makes inside BuildKit is in the 55.55 s and has no bound of
its own: the step's limit is its bound.

## The job's limit, held to the reads it makes

Up to the record step's last read, the job waits on the network in these steps, each counted at
its worst:

- 848 s: `.github/actions/install-crane`, at the worst case `.github/ci-notes/ci.md`'s "The worst
  case of the tool installs that read .github/scripts/ci-tools.sh" derives for an x86_64 runner.
- 245 s: the decide step's `crane config`, through network-install.sh's bound for crane (three
  attempts of 60 s, each killed 15 s after its SIGTERM, with two 10 s pauses).
- 240 s: the builder's pull of its BuildKit image, which setup-buildx-action makes with `docker
  pull` and no bound of its own, counted as the runner's own pulls are (`.github/ci-notes/ci.md`,
  "The runner's own pulls of a job's service and container images").
- 780 s: the build step's own limit, above.
- 735 s: the record step's `crane tag` and its two `crane manifest` reads, 245 s each.
- 20.4 s: every other step at its slowest, from job 128991520632's 97 s: less the build step's
  65.7 s and the four steps above at their healthy 0.6 s, 0.5 s, 8.0 s and 1.8 s.

780 s + 848 s + 245 s + 240 s + 735 s + 20.4 s = 2,868.4 s, rounded up to 48 minutes. The 3
minutes main gives the job would end a stalled read in GitHub's cancellation, which names nothing,
before the wrapper or curl failed it by name.
