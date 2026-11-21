# Notes for the airlock-lean job's workflow

The derivations behind the budgets of `.github/workflows/ci-job-airlock-lean.yaml`, which point
into this file with `# why:` lines, as `.github/ci-notes/README.md` says. Each value's own
arithmetic stays on its line in the workflow file.

## The Lean toolchain step and its bound

`lake build` reads the toolchain `lean-toolchain` names and, where elan holds none of it, has elan
download it first, with no retry: on a miss of the elan cache that download was the one fetch of the
job that no retry covered. So a step of its own installs that toolchain before the build, through
`.github/scripts/ci-retry.sh`, with `elan toolchain install <toolchain>`, the command elan's own help
gives for it (elan 4.2.4, the version of the `elan` and `elan-aarch64` rows of
`.github/ci-tools.yaml`). The build after it finds the toolchain installed and downloads nothing.

What one attempt does, read from elan 4.2.4's source (src/elan-dist/src/manifestation.rs): it
reads the release list at release.lean-lang.org, downloads the archive, unpacks it into a
directory beside the toolchain's and renames that directory into place, so an attempt the bound
kills while it unpacks leaves no toolchain behind, and the next attempt removes the half-unpacked
directory before it unpacks again. The lock elan takes for the install is a file lock, which ends
with the killed process. An attempt that finds the toolchain already there prints
`toolchain '…' is already installed` and exits 0, which is what the step does when the restore
before it hit.

The bound is 120 s an attempt. The archive is `lean-4.35.0-linux_aarch64.tar.zst`, 594,868,592
bytes, and four runs whose elan restore missed (2026-11-17 and 2026-11-18, the first runs on
ubuntu-24.04-arm after main's #4313 moved this job there, which changed the key's architecture)
took, from elan's `info: downloading` line to the line after its install:

- job 134042292619 of run 44083255670: 3.5 s downloading and 11.1 s in all;
- job 134191317349 of run 44126203133: 7.3 s downloading and 14.8 s in all;
- job 134813849343 of run 44304660054: 6.8 s downloading and 13.2 s in all;
- job 134958116292 of run 44348716140, `.github/workflows/ci-caches.yaml`'s elan job: 4.8 s
  downloading and 11.6 s in all.

120 s is eight times the slowest whole install, 14.8 s, and sixteen times its download. The
download is one stream that starts over after a kill, so the bound is set wider than the module
bound's 1.6 times: a `go mod download` resumes from what the attempt before it fetched, and this
does not. Five attempts at that bound with the helper's sleeps between them are
5 x 120 + 150 = 750 s, and the step carries the next whole minute above, 13 minutes.

## The job's budget

The Budgets note of `.github/ci-budgets.md`: the job's measured term, 1200 s, the 20 minutes this
job carried, above the 185 s maximum of the four runs made of it on dispatch://INFRA-1545's
branch (it was selected by no push to main in the window that note names), plus its single
slowest retried step. That is the ci-ok report, 2160 s (that note's "The ci-ok report"), above
install-elan's one `ci_tools_fetch` of the elan tarball, 1708 s, and above the Lean toolchain
step's 750 s. 1200 + 2160 = 3360 s is a whole number of minutes, so the key is the next one
strictly above it, 57 minutes.
