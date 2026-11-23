// packages/cipher-client/src/__tests__/device-login-tester-path.test.ts
//
// A defect the device login's test round found on the real broker (cmd/cipherd through
// scripts/dev-broker.sh, at 62c9f8da2f), pinned as the person meets it: a named pipe (FIFO) at the
// login's path. `dispatch login --device` refuses a directory there before it asks for a code, but
// not a pipe: it asked for a code, the person approved it, and then the command hung with no line
// printed (killed after 60 s), its launcher credential live at the broker and never named, the
// lock left beside the path. The save reads the login it replaces under the lock, and a read of a
// pipe no process writes to never returns.

import { afterEach, expect, test } from "bun:test";
import { closeSync, constants, openSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { STAND_IN_CREDENTIAL, stubDeviceLoginBroker } from "../../test/device-logins";
import { finishDeviceLogin, startDeviceLogin } from "../device-login";
import { DEVICE_LOGIN_FILE_VAR, deviceLoginPlace } from "../device-login-file";

const makeDirectory = temporaryDirectories("device-login-tester-path");

const stops: (() => void)[] = [];
const pipes: string[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
  // A read still blocked on a pipe ends once a writer opens it and closes it again.
  for (const pipe of pipes.splice(0)) {
    try {
      closeSync(openSync(pipe, constants.O_WRONLY | constants.O_NONBLOCK));
    } catch {}
  }
});

/** A named pipe at the login's path, in a directory of this test's own, mode 0700. */
function pipeAtTheLoginPath(): string {
  const dir = join(makeDirectory(), "legion");
  Bun.spawnSync(["mkdir", "-m", "700", "-p", dir]);
  const path = join(dir, "device-login.json");
  const made = Bun.spawnSync(["mkfifo", path]);
  if (made.exitCode !== 0) throw new Error(`mkfifo ${path} failed`);
  pipes.push(path);
  return path;
}

test("a named pipe at the login's path is refused before any code is asked for", async () => {
  const path = pipeAtTheLoginPath();
  const refusal = await deviceLoginPlace(path).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal).not.toBeNull();
  expect(refusal?.message).toContain(DEVICE_LOGIN_FILE_VAR);
});

test("an approved login whose path is a named pipe settles and names its credential", async () => {
  const stub = stubDeviceLoginBroker();
  stops.push(stub.stop);
  const path = pipeAtTheLoginPath();
  const started = await startDeviceLogin({
    brokerUrl: stub.url,
    person: "ada@example.com",
    host: "adas-laptop.local",
  });
  const settled = await finishDeviceLogin(started, { path, sleep: async () => {} }).then(
    () => null,
    (error: unknown) => error as Error
  );
  // Never kept in a pipe: the person is told the credential the broker holds live.
  expect(settled).not.toBeNull();
  expect(settled?.message).toContain(STAND_IN_CREDENTIAL);
}, 10_000);
