# The envoy_refill filter's gate: what job envoy-refill reads. The refill live proof,
# e2e-refill.sh, builds refillrig, the listener and natsd, each on a line of its own, and drives
# them on a NATS server of its own; it reads no other file of the tree. Its step is in the legs of
# envoy-refill-legs (REFILL_LEGS), one per scenario, of the file envoy-refill calls (REFILL_FILE,
# envoy-e2e-local's); the job envoy-refill there only restates their result. So each citation
# names the legs' job, and that file, whose job of that id is not ci.yaml's.
REFILL_FILE = job_file("envoy-refill")
REFILL_LEGS = "envoy-refill-legs"
REFILL_STEP = "Refill live proof"
GATE = gate("envoy_refill", "envoy-refill", runs=[
    script_run(f"{ENVOY}/scripts/e2e-refill.sh", 'go build -o "$rig" ./internal/testnats/cmd/refillrig',
               f"run by {at(REFILL_LEGS, REFILL_STEP, REFILL_FILE)}", ENVOY, ["./internal/testnats/cmd/refillrig"], mode="build"),
    script_run(f"{ENVOY}/scripts/e2e-refill.sh", 'go build -o "${bin_dir}/listener" ./cmd/listener',
               f"run by {at(REFILL_LEGS, REFILL_STEP, REFILL_FILE)}", ENVOY, ["./cmd/listener"], mode="build"),
    script_run(f"{ENVOY}/scripts/e2e-refill.sh", 'go build -o "${bin_dir}/natsd" ./internal/testnats/cmd/natsd',
               f"run by {at(REFILL_LEGS, REFILL_STEP, REFILL_FILE)}", ENVOY, ["./internal/testnats/cmd/natsd"], mode="build"),
], reads=reads(
    (f"{ENVOY}/scripts/e2e-refill.sh", at(REFILL_LEGS, REFILL_STEP, REFILL_FILE)),
) + setup_go(REFILL_LEGS, f"{ENVOY}/go.mod", REFILL_FILE))
