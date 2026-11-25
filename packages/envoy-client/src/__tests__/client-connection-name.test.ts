// A session plugin's NATS connection names its build after its name (`clientBuild`,
// `clientConnectionName`), which legacy-subjects-gate.sh reads from the end of each name in the hub's
// census of connections: the build-commit stamp an image writes beside the plugin's bundle, else a
// release's package version, else none.
import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { connect } from "nats";
import { clientBuild, clientConnectionName, natsConnectionOptions } from "../nats-connection";
import { type AuthHub, startAuthHub } from "./fake-nats-server";

const makeDirectory = temporaryDirectories("client-connection-name");
const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const hubs: AuthHub[] = [];
afterEach(() => {
  for (const hub of hubs.splice(0)) hub.stop();
});

test("the connection name the server sees ends with the build-commit stamp's build", async () => {
  const stamp = join(makeDirectory(), "build-commit");
  writeFileSync(stamp, `${COMMIT}\n`);
  const hub = startAuthHub();
  hubs.push(hub);
  const name = clientConnectionName(
    "omp-ses_name",
    clientBuild(stamp, { name: "@sjawhar/pi-envoy", version: "21.0.0" })
  );
  const nc = await connect(
    natsConnectionOptions({ servers: [hub.url], name, credential: { kind: "none" } })
  );
  await nc.flush();
  await nc.close();
  expect(hub.answered.map((a) => a.name)).toEqual([`omp-ses_name legion@${COMMIT}`]);
});

test("a plugin with no stamp names its release's version, and one run from source names no build", () => {
  const missing = join(makeDirectory(), "build-commit");
  expect(clientBuild(missing, { name: "@sjawhar/claude-envoy", version: "6.0.0" })).toBe(
    "@sjawhar/claude-envoy@6.0.0"
  );
  expect(clientBuild(missing)).toBeUndefined();
  expect(clientConnectionName("claude-envoy-channel-x", clientBuild(missing))).toBe(
    "claude-envoy-channel-x"
  );
});

test("a stamp that is no whole commit, and a version that is no release's, name no build", () => {
  const stamp = join(makeDirectory(), "build-commit");
  for (const written of ["0123456", "main", `${COMMIT}0`, ""]) {
    writeFileSync(stamp, written);
    expect(clientBuild(stamp)).toBeUndefined();
  }
  expect(clientBuild(stamp, { name: "@sjawhar/pi-envoy", version: "21.0.0-rc.1" })).toBeUndefined();
});
