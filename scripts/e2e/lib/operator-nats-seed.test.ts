import { beforeAll, describe, expect, test } from "bun:test";
import { mkdirSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { operatorNatsSeed } from "./operator-nats-seed";

const dir = temporaryDirectory("operator-nats-seed-test");

const production = "nats://envoy-nats.internal.example:4222";
const file = join(dir, "envoy.json");
const cwd = join(dir, "cwd");
const off = join(dir, "off.json");
const token = join(dir, "token");
beforeAll(() => {
  mkdirSync(cwd);
  writeFileSync(
    file,
    JSON.stringify({ envoy: { enabled: true, natsUrls: [production], natsSeed: "SUFILESEED" } })
  );
  writeFileSync(off, JSON.stringify({ envoy: { enabled: false } }));
  writeFileSync(token, "projected.service.account.token\n");
});

describe("operatorNatsSeed", () => {
  test("the file's seed goes to the servers written beside it", () => {
    expect(
      operatorNatsSeed(production, "SMOKE_UPSTREAM_NATS", { ENVOY_CONFIG_FILE: file }, cwd)
    ).toEqual({ value: "SUFILESEED", source: `envoy.natsSeed in ${file}` });
  });

  test("a seed variable wins over the file's, for any server", () => {
    const env = { ENVOY_CONFIG_FILE: file, NATS_NKEY_SEED: "SUVARIABLE" };
    expect(operatorNatsSeed("nats://other.internal.example:4222", "x", env, cwd)).toEqual({
      value: "SUVARIABLE",
      source: "NATS_NKEY_SEED",
    });
  });

  test("the file's seed never goes to another server, and the refusal names the caller's input", () => {
    expect(() =>
      operatorNatsSeed(
        "nats://other.internal.example:4222",
        "SMOKE_UPSTREAM_NATS",
        { ENVOY_CONFIG_FILE: file },
        cwd
      )
    ).toThrow(
      `the NATS seed for SMOKE_UPSTREAM_NATS (resolved as ENVOY_NATS_URL): ENVOY_NATS_URL names other servers than ${file}'s envoy.natsUrls; set NATS_NKEY_SEED_FILE for them`
    );
  });

  test("no seed anywhere is no credential", () => {
    const env = { ENVOY_CONFIG_FILE: join(dir, "absent.json") };
    expect(operatorNatsSeed(production, "x", env, cwd)).toBeUndefined();
  });

  test("a session's broker token is refused, naming the seed to set", () => {
    // The auth service grants a session's token that session's own channels, and these scripts
    // read other subjects and the hub's streams.
    const env = {
      ENVOY_CONFIG_FILE: join(dir, "absent.json"),
      NATS_AUTH: "broker",
      AGENT_SECRETS_URL: "http://broker.test",
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: token,
    };
    expect(() => operatorNatsSeed(production, "SMOKE_UPSTREAM_NATS", env, cwd)).toThrow(
      "the NATS seed for SMOKE_UPSTREAM_NATS (resolved as ENVOY_NATS_URL): NATS_AUTH (or envoy.natsAuth) names a session's broker token, which a hub grants only that session's own channels; set NATS_NKEY_SEED_FILE for SMOKE_UPSTREAM_NATS"
    );
  });

  test("a file that turns Envoy off is refused, saying so", () => {
    expect(() =>
      operatorNatsSeed(production, "<nats-url>", { ENVOY_CONFIG_FILE: off }, cwd)
    ).toThrow(
      `the NATS seed for <nats-url> (resolved as ENVOY_NATS_URL): ${off} sets envoy.enabled to false: Envoy is off`
    );
  });
});
