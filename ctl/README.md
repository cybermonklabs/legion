# Negative controls of INFRA-5580's checks (control-only commit)

Each directory holds a runner (`ctl/run.sh`) and its patches, as they ran at `0fda50f0` and at the
head `ec51dd86`, which adds only test comments. Each patch changes product code only. The runner
applies one patch, runs the check's command after `go test -list`, prints the pass and fail lines,
reverts the patch, and checks that the tree is clean.

The runs were build Jobs made from the cluster's own `legion-remote-test` Job spec, with its
GitHub token volume removed, no pull-request merge, the ref to test in `RT_REF`, and the runner
shipped inside `RT_CMD`, which runs in `packages/daemon` with Go, Postgres and NATS.
