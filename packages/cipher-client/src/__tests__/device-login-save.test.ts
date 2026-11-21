// packages/cipher-client/src/__tests__/device-login-save.test.ts
//
// The save of a new device login (`finishDeviceLogin`), where a step after the person's approval
// fails. The broker holds the login's credential live from the approval on, so a login that is not
// kept is refused naming that credential and where to revoke it, and a copy written whole is kept;
// a login that is kept is answered as kept, whatever fails after its save, so no person is told to
// revoke the login the machine uses. Each row fails one filesystem call (`rename`) by its path.

import { afterEach, expect, spyOn, test } from "bun:test";
import * as fs from "node:fs/promises";
import { readdir, stat } from "node:fs/promises";
import { dirname, join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { stubDeviceLoginBroker } from "../../test/device-logins";
import {
  finishDeviceLogin,
  type KeptDeviceLogin,
  readDeviceLogin,
  startDeviceLogin,
} from "../device-login";

const PERSON = "ada@example.com";
const HOST = "adas-laptop.local";

const makeDirectory = temporaryDirectories("device-login-save");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A stand-in broker stopped after the test. */
function broker(options: Parameters<typeof stubDeviceLoginBroker>[0] = {}) {
  const stub = stubDeviceLoginBroker(options);
  stops.push(stub.stop);
  return stub;
}

/** A device login file's path in a directory of this test's own. */
function loginPath(): string {
  return join(makeDirectory(), "legion", "device-login.json");
}

/** A sleep that waits for nothing, so a poll's interval costs a test no time. */
const noSleep = async (): Promise<void> => {};

/** Fails each `rename` whose source or target `fails` takes, `EIO`, until `restore`. */
function failRenames(fails: (from: string, to: string) => boolean) {
  const real = fs.rename;
  return spyOn(fs, "rename").mockImplementation((async (
    from: Parameters<typeof fs.rename>[0],
    to: Parameters<typeof fs.rename>[1]
  ) => {
    if (fails(String(from), String(to))) {
      throw Object.assign(new Error("refused"), { code: "EIO" });
    }
    return await real(from, to);
  }) as typeof fs.rename);
}

test("an approved login whose save fails once it is written keeps a whole copy, and names it", async () => {
  const stub = broker({ credentialId: "cred-unsaved-1" });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  // The save writes its temporary copy whole, and then the rename over the file fails, as a disk
  // that fails a write or a file this user may not replace fails it.
  const renames = failRenames((_, to) => to === path);
  let unsaved: Error | null;
  try {
    unsaved = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
      () => null,
      (error: unknown) => error as Error
    );
  } finally {
    renames.mockRestore();
  }
  // The broker holds the credential live: the refusal names it, why, and where to revoke it.
  expect(unsaved?.message).toContain("cred-unsaved-1");
  expect(unsaved?.message).toContain("EIO");
  expect(unsaved?.message).toContain(
    `revoke it in Machine logins on ${stub.url}/credentials/machine`
  );
  expect(unsaved?.message).not.toContain(dirname(path));
  // The copy is kept beside the file under the name the refusal gives, 0600, and is the whole
  // login: moved over the file, it is the login.
  const names = await readdir(dirname(path));
  const kept = names.filter((name) => /^device-login\.json\.kept-[0-9a-f-]{36}$/.test(name));
  expect(kept, names.join(", ")).toHaveLength(1);
  const copy = join(dirname(path), kept[0] ?? "");
  expect(unsaved?.message).toContain(copy.slice(path.length));
  expect((await stat(copy)).mode & 0o777).toBe(0o600);
  expect((await readDeviceLogin(copy)).credentialId).toBe("cred-unsaved-1");
  expect(names.filter((name) => name.endsWith(".tmp"))).toEqual([]);
  expect(await stat(path).catch(() => null)).toBeNull();
});

test("a login kept whose lock is not let go is answered as kept, with no revoke", async () => {
  const stub = broker({ credentialId: "cred-kept-1" });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  // The save renames the login over the file; then letting go of the lock fails, at the move of
  // the lock aside.
  const renames = failRenames((from) => from === `${path}.lock`);
  let answer: KeptDeviceLogin | Error;
  try {
    answer = await finishDeviceLogin(started, { path, sleep: noSleep }).catch(
      (error: unknown) => error as Error
    );
  } finally {
    renames.mockRestore();
  }
  // The login is the one the machine now uses: it is answered, not refused, and nothing tells the
  // person to revoke it. Why the lock was left is said, and another process takes it over once it
  // is stale.
  expect(answer).not.toBeInstanceOf(Error);
  const kept = answer as KeptDeviceLogin;
  expect(kept.credentialId).toBe("cred-kept-1");
  expect(kept.lockLeft).toContain("EIO");
  expect(kept.lockLeft).toContain("takes it over once it is 15 s old");
  expect(kept.lockLeft).not.toContain("revoke");
  expect((await readDeviceLogin(path)).credentialId).toBe("cred-kept-1");
});
