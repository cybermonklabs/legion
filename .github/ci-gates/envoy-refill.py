# The envoy_refill filter's gate: what job envoy-refill reads. The refill live proof,
# e2e-refill.sh, builds refillrig, the listener and natsd, each on a line of its own, and drives
# them on a NATS server of its own; it reads no other file of the tree.
GATE = gate("envoy_refill", "envoy-refill", runs=[
    script_run(f"{ENVOY}/scripts/e2e-refill.sh", 'go build -o "$rig" ./internal/testnats/cmd/refillrig',
               f"run by {at('envoy-refill', 'Refill live proof')}", ENVOY, ["./internal/testnats/cmd/refillrig"], mode="build"),
    script_run(f"{ENVOY}/scripts/e2e-refill.sh", 'go build -o "${bin_dir}/listener" ./cmd/listener',
               f"run by {at('envoy-refill', 'Refill live proof')}", ENVOY, ["./cmd/listener"], mode="build"),
    script_run(f"{ENVOY}/scripts/e2e-refill.sh", 'go build -o "${bin_dir}/natsd" ./internal/testnats/cmd/natsd',
               f"run by {at('envoy-refill', 'Refill live proof')}", ENVOY, ["./internal/testnats/cmd/natsd"], mode="build"),
], reads=reads(
    (f"{ENVOY}/scripts/e2e-refill.sh", at("envoy-refill", "Refill live proof")),
) + setup_go("envoy-refill", f"{ENVOY}/go.mod"))
