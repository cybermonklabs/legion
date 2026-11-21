// packages/cipher-client/src/__tests__/device-login-file.test.ts
//
// The file a machine keeps its device login in (`device-login-file.ts`): its read refuses a file
// that is not there, not private, not a regular file or over 64 KiB, in words that never name its
// path, and the save of a new login, where a step after the person's approval fails. The broker
// holds the login's credential live from the approval on, so a login that is not kept is refused
// naming that credential and where to revoke it, and a copy written whole is kept; a login that is
// kept is answered as kept, whatever fails after its save, so no person is told to revoke the login
// the machine uses. A row that fails a step fails one filesystem call by its path (`rename`, `rm`,
// or a read's `readFile` or `close`).

import { afterEach, expect, spyOn, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { once } from "node:events";
import { rmSync } from "node:fs";
import * as fs from "node:fs/promises";
import { chmod, mkdir, readdir, stat } from "node:fs/promises";
import { createServer } from "node:net";
import { basename, dirname, join } from "node:path";
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
import { type DeviceLogin, deviceLoginPlace, readDeviceLogin } from "../device-login-file";
import { DEVICE_LOGIN_STALE_LOCK_MS } from "../device-login-lock";
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

test("a login file swapped for a named pipe after it is looked at is never waited on", async () => {
  // A read that looks at the path and then opens it again could open a named pipe another process
  // put in its place between the two, and wait on it. The file is swapped for a pipe nobody
  // writes to just after the read's first look at the path, whatever that look is: the read must
  // settle, and read the file it looked at.
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  let swapped = false;
  const swap = () => {
    if (swapped) return;
    swapped = true;
    rmSync(path);
    expect(Bun.spawnSync(["mkfifo", path]).exitCode).toBe(0);
  };
  const realStat = fs.stat;
  const realOpen = fs.open;
  const stats = spyOn(fs, "stat").mockImplementation((async (
    ...args: Parameters<typeof fs.stat>
  ) => {
    const info = await realStat(...args);
    if (String(args[0]) === path) swap();
    return info;
  }) as typeof fs.stat);
  const opens = spyOn(fs, "open").mockImplementation((async (
    ...args: Parameters<typeof fs.open>
  ) => {
    const handle = await realOpen(...args);
    if (String(args[0]) === path) swap();
    return handle;
  }) as typeof fs.open);
  let read: DeviceLogin | Error;
  try {
    read = await readDeviceLogin(path).catch((error: unknown) => error as Error);
  } finally {
    stats.mockRestore();
    opens.mockRestore();
  }
  expect(swapped).toBe(true);
  expect(read).not.toBeInstanceOf(Error);
  expect((read as DeviceLogin).credentialId).toBe(STAND_IN_CREDENTIAL);
}, 10_000);

test("a login in a directory others may write is refused before its file is opened", async () => {
  // In such a directory another user picks what is at the path, and the open of some nodes (a
  // device) does something. A named pipe stands in for such a node: the read refuses the
  // directory, and never opens what is at the path.
  const path = loginPath();
  await mkdir(dirname(path), { recursive: true });
  await chmod(dirname(path), 0o777);
  expect(Bun.spawnSync(["mkfifo", path]).exitCode).toBe(0);
  const opened: string[] = [];
  const realOpen = fs.open;
  const opens = spyOn(fs, "open").mockImplementation((async (
    ...args: Parameters<typeof fs.open>
  ) => {
    opened.push(String(args[0]));
    return await realOpen(...args);
  }) as typeof fs.open);
  let refused: Error | null;
  try {
    refused = await readDeviceLogin(path).then(
      () => null,
      (error: unknown) => error as Error
    );
  } finally {
    opens.mockRestore();
  }
  expect(refused?.message).toContain("mode 0777");
  expect(refused?.message).toContain("chmod 700");
  expect(opened).not.toContain(path);
});

test("a login path no open can read is refused in words, not a bare code", async () => {
  // Each is refused by the open itself, before any status is read: a socket (ENXIO), a loop of
  // links (ELOOP), and a path through a file where a directory must be (ENOTDIR).
  const dir = dirname(loginPath());
  await mkdir(dir, { recursive: true, mode: 0o700 });
  const socket = join(dir, "socket.json");
  const server = createServer().listen(socket);
  await once(server, "listening");
  const loop = join(dir, "loop.json");
  await fs.symlink(join(dir, "back.json"), loop);
  await fs.symlink(loop, join(dir, "back.json"));
  const file = join(dir, "a-file");
  await fs.writeFile(file, "", { mode: 0o600 });
  try {
    for (const [path, words] of [
      [socket, "is a socket or a device, not a regular file"],
      [loop, "is a loop of links, not a regular file"],
      [join(file, "device-login.json"), "has a file, not a directory, in its path"],
    ] as const) {
      const refused = await readDeviceLogin(path).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message, path).toContain(words);
      expect(refused?.message, path).not.toMatch(/: E[A-Z]+$/);
      expect(refused?.message, path).not.toContain(dir);
    }
    // The check of the place before a login words the loop the same way.
    const placed = await deviceLoginPlace(loop).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(placed?.message).toContain("is a loop of links, not a regular file");
  } finally {
    server.close();
  }
});

test("a login file over 64 KiB is refused before it is read", async () => {
  // JSON takes white space after its object, so the login padded still parses as the login: only
  // the bound refuses it. At 64 KiB it is read; one byte over, it is refused.
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const login = await fs.readFile(path, "utf8");
  await fs.writeFile(path, login.padEnd(64 * 1024));
  expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
  await fs.writeFile(path, login.padEnd(64 * 1024 + 1));
  const refused = await readDeviceLogin(path).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refused?.message).toContain("over 64 KiB");
  expect(refused?.message).not.toContain(dirname(path));
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
    // The lock is read through a file opened without waiting (`device-login-lock.ts`): the save's
    // look at its lock is the first read of the lock, and the release reads it after.
    const realOpen = fs.open;
    let lockReads = 0;
    const reads = spyOn(fs, "open").mockImplementation((async (
      ...args: Parameters<typeof fs.open>
    ) => {
      const handle = await realOpen(...args);
      if (String(args[0]) === lock) {
        const readHandle = handle.readFile.bind(handle);
        handle.readFile = (async (...options: Parameters<typeof handle.readFile>) => {
          lockReads += 1;
          return lockReads === 1 ? "another process's value\n" : await readHandle(...options);
        }) as typeof handle.readFile;
      }
      return handle;
    }) as typeof fs.open);
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

test("a login whose save keeps its copy and whose lock is then not let go names the copy", async () => {
  const stub = broker({ credentialId: "cred-double-1" });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  // Two failures in one run: the rename over the file fails, so the copy is kept, and then letting
  // go of the lock fails at the move of the lock aside.
  const renames = failRenames((from, to) => to === path || from === `${path}.lock`);
  let unsaved: KeptDeviceLogin | Error;
  try {
    unsaved = await finish(started, path);
  } finally {
    renames.mockRestore();
  }
  expect(unsaved).toBeInstanceOf(Error);
  const refusal = unsaved as Error;
  // The person is told of the copy, which holds the live key and refresh token, and not told the
  // lock is why: the save is.
  expect(refusal.message).toContain("cred-double-1");
  expect(refusal.message).toContain("needs no revoke");
  expect(refusal.message).toContain("delete it, since it holds the login's key and refresh token");
  expect(refusal.message).not.toContain("move the lock");
  expect(refusal.message).not.toContain(dirname(path));
  const left = await leftBeside(path);
  expect(left.kept, left.names.join(", ")).toHaveLength(1);
  expect(refusal.message).toContain((left.kept[0] ?? "").slice(basename(path).length));
  // Control: letting go did fail. The lock is still there, and its failure is kept as the cause
  // of the save's.
  expect(left.names).toContain(`${basename(path)}.lock`);
  const kept = refusal.cause as Error;
  expect(kept.message).toContain(".kept-");
  expect((kept.cause as Error | undefined)?.message).toContain("move the lock beside");
  expect((kept.cause as Error | undefined)?.message).toContain("EIO");
});

/** An error as Bun's fs gives it, its text repeating the path `at` it was handed. */
function pathError(code: string, at: string): Error {
  return Object.assign(new Error(`${code}: ${at}`), { code });
}

test("an fs error of the lock's read or its release never prints the login's path", async () => {
  // The path is the setting a person may have pasted a secret into, so no message names it.
  // The removal of the lock moved aside fails: the login is kept, and the lock left says why.
  {
    const stub = broker({ credentialId: "cred-rm-1" });
    const path = loginPath();
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const realRm = fs.rm;
    const rms = spyOn(fs, "rm").mockImplementation((async (...args: Parameters<typeof fs.rm>) => {
      const at = String(args[0]);
      if (at.startsWith(`${path}.lock.stale-`)) throw pathError("ERR_FS_EISDIR", at);
      return await realRm(...args);
    }) as typeof fs.rm);
    let answer: KeptDeviceLogin | Error;
    try {
      answer = await finish(started, path);
    } finally {
      rms.mockRestore();
    }
    expect(answer).not.toBeInstanceOf(Error);
    const kept = answer as KeptDeviceLogin;
    expect(kept.lockLeft).toContain("ERR_FS_EISDIR");
    expect(kept.lockLeft).not.toContain(dirname(path));
  }
  // The close of each read of the lock fails: the save's look at its lock fails, so the copy is
  // kept, and the refusal says why.
  {
    const stub = broker({ credentialId: "cred-close-1" });
    const path = loginPath();
    const lock = `${path}.lock`;
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const realOpen = fs.open;
    let closes = 0;
    const opens = spyOn(fs, "open").mockImplementation((async (
      ...args: Parameters<typeof fs.open>
    ) => {
      const handle = await realOpen(...args);
      // Only a read of the lock: the lock is made by an open of "wx".
      if (String(args[0]) === lock && typeof args[1] === "number") {
        const close = handle.close.bind(handle);
        handle.close = async () => {
          await close();
          closes += 1;
          throw pathError("EIO", lock);
        };
      }
      return handle;
    }) as typeof fs.open);
    let answer: KeptDeviceLogin | Error;
    try {
      answer = await finish(started, path);
    } finally {
      opens.mockRestore();
    }
    expect(closes).toBeGreaterThan(0);
    expect(answer).toBeInstanceOf(Error);
    const message = (answer as Error).message;
    expect(message).toContain("EIO");
    expect(message).toContain("needs no revoke");
    expect(message).not.toContain(dirname(path));
  }
});

test("a removal of what a save leaves that fails stops no save, and prints no path", async () => {
  const stub = broker({ credentialId: "cred-swept-1" });
  const path = loginPath();
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  // A temporary copy a save that died left, untouched for the stale bound.
  const left = join(dirname(path), `.device-login-${randomUUID()}.json.tmp`);
  await fs.writeFile(left, "{}", { mode: 0o600 });
  const old = new Date(Date.now() - DEVICE_LOGIN_STALE_LOCK_MS - 1_000);
  await fs.utimes(left, old, old);
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  // Each removal of a temporary copy fails, the one left's and the save's own after its rename,
  // with an error that repeats the path.
  const realRm = fs.rm;
  let failed = 0;
  const rms = spyOn(fs, "rm").mockImplementation((async (...args: Parameters<typeof fs.rm>) => {
    const at = String(args[0]);
    if (/^\.device-login-[0-9a-f-]{36}\.json\.tmp$/.test(basename(at))) {
      failed += 1;
      throw pathError("EACCES", at);
    }
    return await realRm(...args);
  }) as typeof fs.rm);
  let answer: KeptDeviceLogin | Error;
  try {
    answer = await finish(started, path);
  } finally {
    rms.mockRestore();
  }
  expect(failed).toBe(2);
  expect(answer).not.toBeInstanceOf(Error);
  expect((answer as KeptDeviceLogin).credentialId).toBe("cred-swept-1");
  expect((await readDeviceLogin(path)).credentialId).toBe("cred-swept-1");
  // What could not be removed is left for a later save to remove.
  expect(await readdir(dirname(path))).toContain(basename(left));
});
