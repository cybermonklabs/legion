// packages/cipher-client/src/__tests__/device-login-file.test.ts
//
// The file a machine keeps its device login in (`device-login-file.ts`): its read refuses a file
// that is not there, not private or not a regular file, and the save of a new login, where a step
// after the person's approval fails. The broker holds the login's credential live from the
// approval on, so a login that is not kept is refused naming that credential and where to revoke
// it, and a copy written whole is kept; a login that is kept is answered as kept, whatever fails
// after its save, so no person is told to revoke the login the machine uses. A row that fails a
// step fails one filesystem call by its path (`rename`, `readFile`).

import { afterEach, expect, spyOn, test } from "bun:test";
import * as fs from "node:fs/promises";
import { chmod, mkdir, readdir, stat } from "node:fs/promises";
import { dirname, join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  STAND_IN_CREDENTIAL,
  stubDeviceLoginBroker,
  writeDeviceLoginFile,
} from "../../test/device-logins";
import {
  type DeviceAuthorization,
  finishDeviceLogin,
  type KeptDeviceLogin,
  startDeviceLogin,
} from "../device-login";
import { readDeviceLogin } from "../device-login-file";
import { DEVICE_LOGIN_COMMAND } from "../token-errors";

const PERSON = "ada@example.com";
const HOST = "adas-laptop.local";

const makeDirectory = temporaryDirectories("device-login-file");

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

/** Fails each `rename` whose source and target `fails` takes, `EIO`, until its `mockRestore`. */
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

/** What a finished login answered, or the error it was refused with. */
async function finish(started: DeviceAuthorization, path: string) {
  return await finishDeviceLogin(started, { path, sleep: noSleep }).catch(
    (error: unknown) => error as Error
  );
}

/** The names in the login's directory: the kept copies, and the temporary copies left. */
async function leftBeside(path: string) {
  const names = await readdir(dirname(path));
  return {
    names,
    kept: names.filter((name) => /^device-login\.json\.kept-[0-9a-f-]{36}$/.test(name)),
    temporary: names.filter((name) => name.endsWith(".tmp")),
  };
}

test("a machine with no login file is told to log in", async () => {
  const refusal = await readDeviceLogin(loginPath()).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toContain("this machine has no device login");
  expect(refusal?.message).toContain(`log this machine in with: ${DEVICE_LOGIN_COMMAND}`);
});

test("a login directory others may write is refused, as a file others may read is", async () => {
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  await chmod(dirname(path), 0o775);
  const refused = await readDeviceLogin(path).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refused?.message).toContain("mode 0775");
  expect(refused?.message).toContain("chmod 700");

  // A login made into such a directory is refused before its file is written there.
  const fresh = join(makeDirectory(), "open", "device-login.json");
  await mkdir(dirname(fresh));
  await chmod(dirname(fresh), 0o777);
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const unsaved = await finishDeviceLogin(started, { path: fresh, sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(unsaved?.message).toContain("chmod 700");
  expect(await stat(fresh).catch(() => null)).toBeNull();

  // A directory others may only read and enter, as `mkdir -p` leaves the `~/.config/legion` that
  // envoy.json lives in, holds a login: its file is 0600, and no one else can replace it there.
  await chmod(dirname(path), 0o755);
  expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
});

test("a login path that is not a regular file is refused before it is read", async () => {
  // A read of a named pipe that no process writes to never returns: the read refuses it, and a
  // link to one, before it opens it. The other end is never opened, so a read would hang the row.
  const pipe = loginPath();
  await mkdir(dirname(pipe), { recursive: true, mode: 0o700 });
  expect(Bun.spawnSync(["mkfifo", pipe]).exitCode).toBe(0);
  const linked = join(dirname(pipe), "linked.json");
  await fs.symlink(pipe, linked);
  for (const path of [pipe, linked]) {
    const refused = await readDeviceLogin(path).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refused?.message, path).toContain("is a named pipe, not a regular file");
    expect(refused?.message, path).not.toContain(dirname(pipe));
  }
});

test("an approved login whose save fails once it is written keeps a whole copy, and names it", async () => {
  const stub = broker({ credentialId: "cred-unsaved-1" });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  // The save writes its temporary copy whole, and then the rename over the file fails, as a disk
  // that fails a write or a file this user may not replace fails it.
  const renames = failRenames((_, to) => to === path);
  let unsaved: KeptDeviceLogin | Error;
  try {
    unsaved = await finish(started, path);
  } finally {
    renames.mockRestore();
  }
  expect(unsaved).toBeInstanceOf(Error);
  const message = (unsaved as Error).message;
  // The broker holds the credential live: the refusal names it and why. The kept copy holds the
  // login, so used it needs no revoke; unused, the person deletes it and revokes the credential.
  expect(message).toContain("cred-unsaved-1");
  expect(message).toContain("EIO");
  expect(message).toContain("needs no revoke");
  expect(message).toContain(
    `delete it, since it holds the login's key and refresh token, and revoke the credential in Machine logins on ${stub.url}/credentials/machine`
  );
  expect(message).not.toContain(dirname(path));
  // The copy is kept beside the file under the name the refusal gives, 0600, and is the whole
  // login: moved over the file, it is the login.
  const left = await leftBeside(path);
  expect(left.kept, left.names.join(", ")).toHaveLength(1);
  const copy = join(dirname(path), left.kept[0] ?? "");
  expect(message).toContain(copy.slice(path.length));
  expect((await stat(copy)).mode & 0o777).toBe(0o600);
  expect((await readDeviceLogin(copy)).credentialId).toBe("cred-unsaved-1");
  expect(left.temporary).toEqual([]);
  expect(await stat(path).catch(() => null)).toBeNull();
});

test("an approved login whose lock was lost during its save keeps a whole copy, and says why", async () => {
  // The save writes its copy whole, and then reads its lock again: another process's value is
  // there, as after a laptop asleep for over 15 s during the save. The copy is kept, and, where
  // even its own rename fails, nothing is left but the refusal.
  for (const keptCopyFails of [false, true]) {
    const stub = broker({ credentialId: "cred-lost-1" });
    const path = loginPath();
    const lock = `${path}.lock`;
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const real = fs.readFile;
    let lockReads = 0;
    const reads = spyOn(fs, "readFile").mockImplementation((async (
      ...args: Parameters<typeof fs.readFile>
    ) => {
      // The save's look at its lock is the first read of the lock: the release reads it after.
      if (String(args[0]) === lock) {
        lockReads += 1;
        if (lockReads === 1) return "another process's value\n";
      }
      return await real(...args);
    }) as typeof fs.readFile);
    const renames = failRenames((_, to) => keptCopyFails && to.startsWith(`${path}.kept-`));
    let lost: KeptDeviceLogin | Error;
    try {
      lost = await finish(started, path);
    } finally {
      reads.mockRestore();
      renames.mockRestore();
    }
    expect(lost, `kept copy fails: ${keptCopyFails}`).toBeInstanceOf(Error);
    const message = (lost as Error).message;
    expect(message).toContain("cred-lost-1");
    expect(message).toContain("the new login was not kept in its place");
    const left = await leftBeside(path);
    expect(left.temporary, left.names.join(", ")).toEqual([]);
    expect(await stat(path).catch(() => null)).toBeNull();
    if (keptCopyFails) {
      expect(left.kept, left.names.join(", ")).toEqual([]);
      expect(message).not.toContain(".kept-");
      expect(message).toContain(
        `revoke it in Machine logins on ${stub.url}/credentials/machine, and log in again`
      );
    } else {
      expect(left.kept, left.names.join(", ")).toHaveLength(1);
      expect((await readDeviceLogin(join(dirname(path), left.kept[0] ?? ""))).credentialId).toBe(
        "cred-lost-1"
      );
      expect(message).toContain("needs no revoke");
    }
  }
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
    answer = await finish(started, path);
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
