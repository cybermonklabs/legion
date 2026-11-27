# The envoy_e2e_local filter's gate: what job envoy-e2e-local reads.
GATE = gate("envoy_e2e_local", "envoy-e2e-local", runs=[
    script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$listener_binary" ./cmd/listener',
               f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./cmd/listener"], mode="build"),
    script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$nats_binary" ./internal/testnats/cmd/natsd',
               f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./internal/testnats/cmd/natsd"], mode="build"),
    script_run(f"{ENVOY}/scripts/e2e-local.sh", 'go build -o "$fixturebuckets_binary" ./internal/testnats/cmd/fixturebuckets',
               f"run by {at('envoy-e2e-local', 'Local end-to-end acceptance')}", ENVOY, ["./internal/testnats/cmd/fixturebuckets"], mode="build"),
], reads=reads(
    (f"{ENVOY}/scripts/e2e-local.test.sh", at("envoy-e2e-local", "Refusal test for e2e-local.sh")),
    (f"{ENVOY}/scripts/e2e-local.sh", f"{at('envoy-e2e-local', 'Local end-to-end acceptance')}; `packages/envoy/scripts/e2e-local.test.sh#driver` runs it"),
    (f"{ENVOY}/scripts/fixtures/github/**", "`packages/envoy/scripts/e2e-local.sh#post_github` posts each fixture"),
    # envoy-client's sources, and cipher-client's with them, are the shared read below.
    (CONTRACTS_SRC, "packages/envoy-client/src/delivery.ts imports @legion/contracts"),
) + envoy_client_dep("`packages/envoy/scripts/e2e-local.sh#renderInbound` and `packages/envoy/scripts/e2e-local.sh#createEnvoyClient` runs bun in packages/envoy-client "
                     "importing @legion/envoy-client/delivery and @legion/envoy-client/transport")
  + setup_go("envoy-e2e-local", f"{ENVOY}/go.mod") + bun_job("envoy-e2e-local"))
