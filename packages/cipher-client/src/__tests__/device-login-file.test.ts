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
import * as os from "node:os";
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
  // A read of a named pipe that no process writes to never returns: the read opens it, and a link
  // to one, without waiting, and refuses it on the open file before any read of it. No process
  // opens it to write, so a read of it would hang the row.
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

/** Records the path of each `open`, and of each `stat` and `lstat`, until `restore`: what a read
 *  opened, and what it looked at by its path. */
function watchOpensAndLooks() {
  const opened: string[] = [];
  const looked: string[] = [];
  const realOpen = fs.open;
  const opens = spyOn(fs, "open").mockImplementation((async (
    ...args: Parameters<typeof fs.open>
  ) => {
    opened.push(String(args[0]));
    return await realOpen(...args);
  }) as typeof fs.open);
  const realStat = fs.stat;
  const stats = spyOn(fs, "stat").mockImplementation((async (
    ...args: Parameters<typeof fs.stat>
  ) => {
    looked.push(String(args[0]));
    return await realStat(...args);
  }) as typeof fs.stat);
  const realLstat = fs.lstat;
  const lstats = spyOn(fs, "lstat").mockImplementation((async (
    ...args: Parameters<typeof fs.lstat>
  ) => {
    looked.push(String(args[0]));
    return await realLstat(...args);
  }) as typeof fs.lstat);
  return {
    opened,
    looked,
    /** Empties both records. */
    clear: () => {
      opened.length = 0;
      looked.length = 0;
    },
    restore: () => {
      opens.mockRestore();
      stats.mockRestore();
      lstats.mockRestore();
    },
  };
}

test("a login reached through a link into a directory others may write is refused before it is opened", async () => {
  // The setting may name a link. The file is then in the directory the link leads into, where
  // another user who can write picks what the open opens, as in the login's own directory.
  const stub = broker();
  const path = loginPath();
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const target = join(makeDirectory(), "shared", "device-login.json");
  await writeDeviceLoginFile(target, stub.url);
  await fs.symlink(target, path);
  const watch = watchOpensAndLooks();
  const { opened, looked } = watch;
  try {
    // Writable by anyone, and by its group. The file the link names is neither opened nor looked
    // at before its directory is refused.
    for (const mode of [0o777, 0o770]) {
      await chmod(dirname(target), mode);
      const refused = await readDeviceLogin(path).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message).toContain(`mode ${mode.toString(8).padStart(4, "0")}`);
      expect(refused?.message).toContain("links to");
      expect(refused?.message).toContain("chmod 700");
      expect(refused?.message).not.toContain(dirname(target));
      expect(opened).not.toContain(target);
      expect(looked).not.toContain(target);
    }
    expect(opened).not.toContain(path);
    // Control: the same link into a directory others may only read and enter is read, and its
    // open is one these spies see.
    await chmod(dirname(target), 0o755);
    expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
    expect(opened).toContain(path);
    // Through a chain of links, the first one relative to its own directory, the directory of the
    // file the last one names is the one checked, and that file is neither opened nor looked at.
    const hop = join(dirname(dirname(path)), "hop");
    await mkdir(hop, { mode: 0o700 });
    await fs.symlink(target, join(hop, "next.json"));
    await fs.rm(path);
    await fs.symlink(join("..", "hop", "next.json"), path);
    await chmod(dirname(target), 0o777);
    watch.clear();
    const chained = await readDeviceLogin(path).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(chained?.message).toContain("mode 0777");
    expect(chained?.message).toContain("links to");
    expect(opened).not.toContain(target);
    expect(looked).not.toContain(target);
    // Control: the spies see the walk's look at the middle link.
    expect(looked.some((at) => at.endsWith(join("hop", "next.json")))).toBe(true);
    await chmod(dirname(target), 0o755);
    expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
  } finally {
    watch.restore();
  }
});

test("a login reached through a link in a directory others may write is refused before it is opened", async () => {
  // A link in the middle of a chain sits in a directory of its own: another user who can write
  // there replaces that link, and with it picks what the open opens.
  const stub = broker();
  const path = loginPath();
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const middle = join(makeDirectory(), "middle", "device-login.json");
  await mkdir(dirname(middle), { mode: 0o700 });
  const target = join(makeDirectory(), "real", "device-login.json");
  await writeDeviceLoginFile(target, stub.url);
  await fs.symlink(target, middle);
  await fs.symlink(middle, path);
  const watch = watchOpensAndLooks();
  const { opened, looked } = watch;
  try {
    await chmod(dirname(middle), 0o777);
    const refused = await readDeviceLogin(path).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refused?.message).toContain("mode 0777");
    expect(refused?.message).toContain("links to");
    expect(refused?.message).toContain("chmod 700");
    expect(refused?.message).not.toContain(dirname(middle));
    // Neither the middle link nor the file at the end is opened or looked at.
    for (const at of [path, middle, target]) expect(opened).not.toContain(at);
    for (const at of [middle, target]) expect(looked).not.toContain(at);
    // Control: the same chain with every directory others may at most read and enter is read,
    // and the spies see its open and the walk's look at the middle link.
    await chmod(dirname(middle), 0o755);
    expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
    expect(opened).toContain(path);
    expect(looked).toContain(middle);
  } finally {
    watch.restore();
  }
});

/**
 * A login in a directory of this user's own (`real`), reached through `shared/sub`, a link to
 * `real` in a directory `shared` of its own, in the two ways a setting can: by a path under that
 * link, and by a link (in a third directory, `own`) to such a path. Answers `shared`, the login's
 * file and the two settings.
 */
async function throughADirectoryLink(broker: string) {
  const base = makeDirectory();
  const shared = join(base, "shared");
  await mkdir(shared, { mode: 0o700 });
  const login = join(base, "real", "device-login.json");
  await writeDeviceLoginFile(login, broker);
  await fs.symlink(dirname(login), join(shared, "sub"));
  const underTheLink = join(shared, "sub", "device-login.json");
  const own = join(base, "own");
  await mkdir(own, { mode: 0o700 });
  const toTheLink = join(own, "device-login.json");
  await fs.symlink(underTheLink, toTheLink);
  return { shared, login, settings: [underTheLink, toTheLink] };
}

test("a login reached through a directory link in a directory others may write is refused before it is opened", async () => {
  // A directory link sits in the directory above it: another user who can write there replaces
  // the link, and with it the directory the login is read from. Every directory on the way is
  // checked (`checkDirectoriesOnTheWay` says which of its rules are OpenSSH's and which its own).
  const stub = broker();
  const { shared, login, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o777);
  const watch = watchOpensAndLooks();
  try {
    for (const setting of settings) {
      const refused = await readDeviceLogin(setting).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message, setting).toContain("mode 0777");
      expect(refused?.message, setting).toContain("on the way to");
      expect(refused?.message, setting).not.toContain(shared);
    }
    for (const at of [...settings, login]) expect(watch.opened).not.toContain(at);
    // Control: with that directory 0755 both are read, and the spies see the read's open.
    await chmod(shared, 0o755);
    for (const setting of settings) {
      expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
    }
    expect(watch.opened).toContain(settings[0]);
  } finally {
    watch.restore();
  }
});

test("a directory on the way to the login that neither this user nor root owns is refused", async () => {
  // Its owner may replace what is in it whatever its mode. No test can make a directory another
  // user owns, so the one on the way reads as owned by such a user.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o755);
  // The walk names each directory by the way it reached it, which is its real path.
  const reached = await fs.realpath(shared);
  const other = (process.getuid?.() ?? 0) + 4242;
  const realLstat = fs.lstat;
  const lstats = spyOn(fs, "lstat").mockImplementation((async (
    ...args: Parameters<typeof fs.lstat>
  ) => {
    const info = await realLstat(...args);
    if (String(args[0]) !== reached) return info;
    return new Proxy(info, {
      get: (seen, key) => (key === "uid" ? other : Reflect.get(seen, key)),
    });
  }) as typeof fs.lstat);
  try {
    for (const setting of settings) {
      const refused = await readDeviceLogin(setting).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message, setting).toContain(`owned by uid ${other}, neither this user`);
      expect(refused?.message, setting).toContain("on the way to");
    }
  } finally {
    lstats.mockRestore();
  }
  // Control: owned by this user, the same directory is no refusal.
  for (const setting of settings) {
    expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
  }
});

test("a directory others may write with the sticky bit, as /tmp is, is no refusal on the way to the login", async () => {
  // In a sticky directory only an entry's owner, or root, can remove or rename it, so no other user
  // replaces the link there.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o1777);
  for (const setting of settings) {
    expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
  }
});

test("a link on the way that another user owns is refused, in a sticky directory too", async () => {
  // Its owner can remove it and make it again naming another place, sticky bit or not, between the
  // walk and the open. No test can make a link another user owns, so the link reads as owned by
  // such a user.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o1777);
  const link = join(await fs.realpath(shared), "sub");
  const other = (process.getuid?.() ?? 0) + 4242;
  const realLstat = fs.lstat;
  const lstats = spyOn(fs, "lstat").mockImplementation((async (
    ...args: Parameters<typeof fs.lstat>
  ) => {
    const info = await realLstat(...args);
    if (String(args[0]) !== link) return info;
    return new Proxy(info, {
      get: (seen, key) => (key === "uid" ? other : Reflect.get(seen, key)),
    });
  }) as typeof fs.lstat);
  try {
    for (const setting of settings) {
      const refused = await readDeviceLogin(setting).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message, setting).toContain(`a link on the way to`);
      expect(refused?.message, setting).toContain(`owned by uid ${other}, neither this user`);
    }
  } finally {
    lstats.mockRestore();
  }
  // Control: the same link of this user's own, in the same sticky directory, is read.
  for (const setting of settings) {
    expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
  }
});

test("a link on the way whose readlink keeps failing as replaced stops at the bound of links", async () => {
  // A link replaced between its lstat and its readlink is looked at again, and each look counts
  // toward the bound, so a link replaced over and over cannot hold the read. The spy gives the
  // real target after 100 failures, so a walk that counts no look reads the login.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o755);
  const link = join(await fs.realpath(shared), "sub");
  let reads = 0;
  const realReadlink = fs.readlink;
  const readlinks = spyOn(fs, "readlink").mockImplementation((async (
    ...args: Parameters<typeof fs.readlink>
  ) => {
    if (String(args[0]) === link && reads < 100) {
      reads += 1;
      throw Object.assign(new Error("not a link"), { code: "EINVAL" });
    }
    return await realReadlink(...args);
  }) as typeof fs.readlink);
  let refused: Error | null;
  try {
    refused = await readDeviceLogin(settings[0] ?? "").then(
      () => null,
      (error: unknown) => error as Error
    );
  } finally {
    readlinks.mockRestore();
  }
  expect(refused?.message).toContain("is a loop of links, not a regular file");
  expect(reads).toBe(40);
});

/**
 * The users and groups files (`/etc/passwd`, `/etc/group`) of a machine where `gid` is this
 * user's private group: this user, `me`, has it as their primary group, and no one else is in it,
 * and `home`, else `/home/me`, as their home directory. Another user, `other`, has a private group
 * of their own. `more` adds lines to each file.
 */
function usersAndGroups(
  gid: number,
  more: { passwd?: string; group?: string; home?: string } = {}
) {
  const me = process.getuid?.() ?? 0;
  return {
    "/etc/passwd": `me:x:${me}:${gid}::${more.home ?? "/home/me"}:/bin/sh\nother:x:${me + 4242}:${gid + 4243}::/home/other:/bin/sh\n${more.passwd ?? ""}`,
    "/etc/group": `me:x:${gid}:\nother:x:${gid + 4243}:\n${more.group ?? ""}`,
  };
}

test("a directory on the way that this user's private group may write, as a Legion pod's /legion is, is no refusal", async () => {
  // A pod's /legion is 2775, of group legion, which has no member but the pod's user: the user
  // private group of Debian's OpenSSH (its user-group-modes patch). No other user can write to it.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o2775);
  const { gid } = await stat(shared);
  const files = readsSystemFiles(usersAndGroups(gid));
  try {
    for (const setting of settings) {
      expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
    }
  } finally {
    files.restore();
  }
  // Control: the walk read the users and groups to take it.
  expect(files.reads()).toBeGreaterThan(0);
});

test("the users, the groups and the uid map are read once while they stay as they are, and again once one changes", async () => {
  // A read makes several walks, and a walk looks at them at each directory its group may write
  // or the overflow uid owns, as at a pod's /legion: each look after the first costs a stat.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o2775);
  const { gid } = await stat(shared);
  const owners = lstatReports({ uid: 65534 }, [await fs.realpath(shared)]);
  const files = readsSystemFiles({
    ...usersAndGroups(gid),
    "/proc/self/uid_map": "         0       1000          1\n",
    "/proc/sys/kernel/overflowuid": "65534\n",
  });
  try {
    for (const setting of settings) {
      expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
    }
    // Control: the reads looked at each of them, and read each once.
    for (const at of [
      "/etc/passwd",
      "/etc/group",
      "/proc/self/uid_map",
      "/proc/sys/kernel/overflowuid",
    ]) {
      expect(files.reads(at), at).toBe(1);
    }
    for (const setting of settings) {
      expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
    }
    expect(files.reads()).toBe(4);
    // An /etc/group that now lists another member of the group is read again, and refused.
    files.change("/etc/group", `me:x:${gid}:other\nother:x:${gid + 4243}:\n`);
    const refused = await readDeviceLogin(settings[0] ?? "").then(
      () => null,
      (error: unknown) => (error as Error).message
    );
    expect(refused).toContain(`mode 2775, and its group, gid ${gid}`);
    expect(files.reads("/etc/group")).toBe(2);
    expect(files.reads()).toBe(5);
  } finally {
    files.restore();
    owners.restore();
  }
});

/**
 * Reads each setting with the users and groups files `files` holds and `shared` (from
 * {@link throughADirectoryLink}) reading as of group `gid`, and answers each setting's refusal, or
 * null where it was read.
 */
async function readWithGroup(
  settings: readonly string[],
  shared: string,
  gid: number,
  files: Readonly<Record<string, string | null>>
): Promise<(string | null)[]> {
  const groups = lstatReports({ gid }, [await fs.realpath(shared)]);
  const reads = readsSystemFiles(files);
  try {
    const refusals: (string | null)[] = [];
    for (const setting of settings) {
      refusals.push(
        await readDeviceLogin(setting).then(
          () => null,
          (error: unknown) => (error as Error).message
        )
      );
    }
    return refusals;
  } finally {
    groups.restore();
    reads.restore();
  }
}

test("a directory on the way that its group may write is refused where the group lists a member other than this user", async () => {
  // That member can replace what is in it, as anyone can in a 0777 one.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o775);
  const { gid } = await stat(shared);
  const team = gid + 4244;
  for (const refusal of await readWithGroup(
    settings,
    shared,
    team,
    usersAndGroups(gid, { group: `team:x:${team}:me,other\n` })
  )) {
    expect(refusal).toContain(`mode 0775, and its group, gid ${team}`);
    expect(refusal).toContain("on the way to");
  }
  // Control: with this user its only member, the same group's directory is read.
  expect(
    await readWithGroup(
      settings,
      shared,
      team,
      usersAndGroups(gid, { group: `team:x:${team}:me\n` })
    )
  ).toEqual([null, null]);
});

test("a directory on the way that its group may write is refused where the group is another user's primary group", async () => {
  // /etc/group lists no member by a primary group: /etc/passwd names it as that user's.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o775);
  const { gid } = await stat(shared);
  const team = gid + 4244;
  const intruder = (process.getuid?.() ?? 0) + 4245;
  for (const refusal of await readWithGroup(
    settings,
    shared,
    team,
    usersAndGroups(gid, {
      passwd: `intruder:x:${intruder}:${team}::/home/intruder:/bin/sh\n`,
      group: `team:x:${team}:me\n`,
    })
  )) {
    expect(refusal).toContain(`mode 0775, and its group, gid ${team}`);
    expect(refusal).toContain("on the way to");
  }
  // Control: with that user's primary group another, the same group's directory is read.
  expect(
    await readWithGroup(
      settings,
      shared,
      team,
      usersAndGroups(gid, {
        passwd: `intruder:x:${intruder}:${intruder}::/home/intruder:/bin/sh\n`,
        group: `team:x:${team}:me\n`,
      })
    )
  ).toEqual([null, null]);
});

test("a directory on the way that its group may write is refused where the users or groups cannot be read, or its group is unknown or has no member", async () => {
  // What cannot be read cannot show the group is this user's alone, so the walk fails closed. A
  // group with no member at all is not this user's either (a setgid program's, say).
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o2775);
  const { gid } = await stat(shared);
  const team = gid + 4244;
  const known = usersAndGroups(gid, { group: `team:x:${team}:me\n` });
  const nis = (process.getuid?.() ?? 0) + 4246;
  for (const [files, why] of [
    [{ ...known, "/etc/group": null }, "/etc/group refused"],
    [{ ...known, "/etc/passwd": null }, "/etc/passwd refused"],
    [
      { ...usersAndGroups(team), "/etc/group": usersAndGroups(gid)["/etc/group"] },
      "team, this user's primary group, not in /etc/group",
    ],
    [usersAndGroups(gid, { group: `team:x:${team}:\n` }), "team has no member"],
    [
      usersAndGroups(gid, { passwd: `+:x:${nis}:${nis}:::\n`, group: `team:x:${team}:me\n` }),
      "a NIS line in /etc/passwd",
    ],
  ] as const) {
    for (const refusal of await readWithGroup(settings, shared, team, files)) {
      // Where a file cannot be read, a group-writable directory above `shared`, as a Legion pod's
      // /legion is, is refused first, so the gid named is not always `team`.
      expect(refusal, why).toMatch(/mode 2775, and its group, gid \d+, is not one/);
      expect(refusal, why).toContain("on the way to");
    }
  }
  // Control: with both read and the group named, the same directory is read.
  expect(await readWithGroup(settings, shared, team, known)).toEqual([null, null]);
});

test("a directory others may write inside an absolute link's target is refused", async () => {
  // A link whose target is absolute is walked from `/`: the directories of its target are on the
  // way, and here one of them is one others may write to.
  const stub = broker();
  const base = makeDirectory();
  const open = join(base, "open");
  const login = join(open, "inner", "device-login.json");
  await writeDeviceLoginFile(login, stub.url);
  const own = join(base, "own");
  await mkdir(own, { mode: 0o700 });
  await fs.symlink(dirname(login), join(own, "hop"));
  await chmod(open, 0o777);
  const path = join(own, "hop", "device-login.json");
  const refused = await readDeviceLogin(path).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refused?.message).toContain("mode 0777");
  expect(refused?.message).toContain("on the way to");
  // Control: with that directory 0755 the same path is read.
  await chmod(open, 0o755);
  expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
});

test("a link on the way that root owns is no refusal", async () => {
  // A system's own links are root's, as macOS's /var -> private/var and /tmp -> private/tmp are,
  // and root can change any file anyway. The link reads as root's.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o755);
  const link = join(await fs.realpath(shared), "sub");
  let lookedAt = 0;
  const realLstat = fs.lstat;
  const lstats = spyOn(fs, "lstat").mockImplementation((async (
    ...args: Parameters<typeof fs.lstat>
  ) => {
    const info = await realLstat(...args);
    if (String(args[0]) !== link) return info;
    lookedAt += 1;
    return new Proxy(info, {
      get: (seen, key) => (key === "uid" ? 0 : Reflect.get(seen, key)),
    });
  }) as typeof fs.lstat);
  try {
    for (const setting of settings) {
      expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
    }
  } finally {
    lstats.mockRestore();
  }
  // Control: the walk did look at the link, as root's.
  expect(lookedAt).toBeGreaterThan(0);
});

/** Makes `lstat` report `reports`' owner uid and group gid, where it names them, for each path in
 *  `at`, until `restore`. */
function lstatReports(
  reports: { readonly uid?: number; readonly gid?: number },
  at: readonly string[]
) {
  const realLstat = fs.lstat;
  const lstats = spyOn(fs, "lstat").mockImplementation((async (
    ...args: Parameters<typeof fs.lstat>
  ) => {
    const info = await realLstat(...args);
    if (!at.includes(String(args[0]))) return info;
    return new Proxy(info, {
      get: (seen, key) =>
        key === "uid" && reports.uid !== undefined
          ? reports.uid
          : key === "gid" && reports.gid !== undefined
            ? reports.gid
            : Reflect.get(seen, key),
    });
  }) as typeof fs.lstat);
  return { restore: () => lstats.mockRestore() };
}

/** A number no file's status holds, new for each file {@link readsSystemFiles} stands in for and
 *  for each change of one, so the file reads as one changed since any status seen before. */
let systemFileChange = 0;

/**
 * Makes this process read each file `given` names as the text it gives, or as refused (`EACCES`)
 * where it gives null, with a status of its own, until `restore`. `change` gives a file new text
 * and a new status, as an edit of it does. `reads` answers how many reads of those files there
 * were, or of the file `at` alone.
 */
function readsSystemFiles(given: Readonly<Record<string, string | null>>) {
  const files = new Map(
    Object.entries(given).map(([at, text]) => [at, { text, change: ++systemFileChange }])
  );
  const reads = new Map<string, number>();
  const refused = () => Object.assign(new Error("refused"), { code: "EACCES" });
  const realReadFile = fs.readFile;
  const readSpy = spyOn(fs, "readFile").mockImplementation((async (
    ...args: Parameters<typeof fs.readFile>
  ) => {
    const at = String(args[0]);
    const file = files.get(at);
    if (file === undefined) return await realReadFile(...args);
    reads.set(at, (reads.get(at) ?? 0) + 1);
    if (file.text === null) throw refused();
    return file.text;
  }) as typeof fs.readFile);
  const realStat = fs.stat;
  const statSpy = spyOn(fs, "stat").mockImplementation((async (
    ...args: Parameters<typeof fs.stat>
  ) => {
    const file = files.get(String(args[0]));
    if (file === undefined) return await realStat(...args);
    if (file.text === null) throw refused();
    const { change } = file;
    return { dev: -1, ino: change, size: file.text.length, mtimeMs: change, ctimeMs: change };
  }) as typeof fs.stat);
  return {
    change: (at: string, text: string) => files.set(at, { text, change: ++systemFileChange }),
    reads: (at?: string) =>
      at === undefined
        ? [...reads.values()].reduce((sum, count) => sum + count, 0)
        : (reads.get(at) ?? 0),
    restore: () => {
      readSpy.mockRestore();
      statSpy.mockRestore();
    },
  };
}

/** Makes this process read `uidMap` as its `/proc/self/uid_map`, and 65534 as the overflow uid,
 *  until `restore`. */
function readsUidMap(uidMap: string) {
  return readsSystemFiles({
    "/proc/self/uid_map": uidMap,
    "/proc/sys/kernel/overflowuid": "65534\n",
  });
}

/** Makes `os.userInfo()` answer `home` as this user's home directory, until `restore`. */
function homeIs(home: string) {
  const realUserInfo = os.userInfo;
  const spy = spyOn(os, "userInfo").mockImplementation(((
    ...args: Parameters<typeof os.userInfo>
  ) => ({ ...realUserInfo(...args), homedir: home })) as typeof os.userInfo);
  return { restore: () => spy.mockRestore() };
}

test("in a user namespace other than the first, a directory on the way the overflow uid owns is root's", async () => {
  // In a gVisor sandbox or a rootless container, root's own directories show as the overflow uid.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o755);
  const owners = lstatReports({ uid: 65534 }, [await fs.realpath(shared)]);
  const namespace = readsUidMap("         0       1000          1\n");
  try {
    for (const setting of settings) {
      expect((await readDeviceLogin(setting)).credentialId).toBe(STAND_IN_CREDENTIAL);
    }
  } finally {
    owners.restore();
    namespace.restore();
  }
});

test("in the first user namespace, a directory on the way the overflow uid owns is refused", async () => {
  // There the overflow uid is a user's of its own (nobody's), not root's.
  const stub = broker();
  const { shared, settings } = await throughADirectoryLink(stub.url);
  await chmod(shared, 0o755);
  const owners = lstatReports({ uid: 65534 }, [await fs.realpath(shared)]);
  const namespace = readsUidMap("         0          0 4294967295\n");
  try {
    for (const setting of settings) {
      const refused = await readDeviceLogin(setting).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message, setting).toContain("owned by uid 65534, neither this user");
    }
  } finally {
    owners.restore();
    namespace.restore();
  }
});

test("a login under the home directory is read whoever owns the directories above it", async () => {
  // They hold the home directory itself: as OpenSSH does, the walk checks the home directory and
  // what is below it, and nothing above it, which in a container can be any owner's. The home
  // directory is this user's in /etc/passwd, as OpenSSH's is, not the HOME the process started
  // with, which Bun's `os.userInfo().homedir` answers.
  const stub = broker();
  const base = makeDirectory();
  const home = join(base, "home");
  const elsewhere = join(base, "elsewhere");
  const path = join(home, ".config", "legion", "device-login.json");
  await writeDeviceLoginFile(path, stub.url);
  const other = (process.getuid?.() ?? 0) + 4242;
  const gid = process.getgid?.() ?? 0;
  const owners = lstatReports({ uid: other }, [await fs.realpath(base)]);
  const files = readsSystemFiles(usersAndGroups(gid, { home }));
  try {
    const user = homeIs(elsewhere);
    try {
      expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
    } finally {
      user.restore();
    }
    // Control: with this user's home directory elsewhere in /etc/passwd, the same directory is on
    // the way, and refused, whatever `os.userInfo()` answers.
    files.change("/etc/passwd", usersAndGroups(gid, { home: elsewhere })["/etc/passwd"]);
    const atHome = homeIs(home);
    try {
      const refused = await readDeviceLogin(path).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message).toContain(`owned by uid ${other}, neither this user`);
    } finally {
      atHome.restore();
    }
  } finally {
    files.restore();
    owners.restore();
  }
});

test("where /etc/passwd names no user of this uid, the home directory is the one os.userInfo() answers", async () => {
  // A container can run as a uid its /etc/passwd does not name.
  const stub = broker();
  const base = makeDirectory();
  const home = join(base, "home");
  const path = join(home, ".config", "legion", "device-login.json");
  await writeDeviceLoginFile(path, stub.url);
  const other = (process.getuid?.() ?? 0) + 4242;
  const gid = process.getgid?.() ?? 0;
  const owners = lstatReports({ uid: other }, [await fs.realpath(base)]);
  const files = readsSystemFiles({
    "/etc/passwd": `other:x:${other}:${gid + 4243}::/home/other:/bin/sh\n`,
    "/etc/group": usersAndGroups(gid)["/etc/group"],
  });
  try {
    const user = homeIs(home);
    try {
      expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
    } finally {
      user.restore();
    }
    // Control: with `os.userInfo()` answering another home directory, the directories above this
    // one are on the way, and refused.
    const elsewhere = homeIs(join(base, "elsewhere"));
    try {
      const refused = await readDeviceLogin(path).then(
        () => null,
        (error: unknown) => error as Error
      );
      expect(refused?.message).toContain("on the way to");
    } finally {
      elsewhere.restore();
    }
  } finally {
    files.restore();
    owners.restore();
  }
});

test("the home directory itself is on the way to a login under it", async () => {
  // Others who may write to it could replace ~/.config, and with it the login.
  const stub = broker();
  const home = join(makeDirectory(), "home");
  const path = join(home, ".config", "legion", "device-login.json");
  await writeDeviceLoginFile(path, stub.url);
  const files = readsSystemFiles(usersAndGroups(process.getgid?.() ?? 0, { home }));
  try {
    await chmod(home, 0o777);
    const refused = await readDeviceLogin(path).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refused?.message).toContain("mode 0777");
    expect(refused?.message).toContain("on the way to");
    // Control: with the home directory 0755, the same login is read.
    await chmod(home, 0o755);
    expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
  } finally {
    files.restore();
  }
});

test("a relative link in a directory reached through a link climbs from where the link really is", async () => {
  // The system resolves a `..` in a link's target from the directory the link is really in. Here
  // the login's directory is a link, so `..` from it is not what the path's own spelling says.
  const stub = broker();
  const base = makeDirectory();
  const real = join(base, "deep", "legion");
  await mkdir(real, { recursive: true, mode: 0o700 });
  const target = join(base, "deep", "hop", "next.json");
  await writeDeviceLoginFile(target, stub.url);
  await fs.symlink(real, join(base, "legion"));
  const path = join(base, "legion", "device-login.json");
  await fs.symlink(join("..", "hop", "next.json"), path);
  expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
  // The directory checked is the one the file is really in.
  await chmod(dirname(target), 0o777);
  const refused = await readDeviceLogin(path).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refused?.message).toContain("mode 0777");
  expect(refused?.message).toContain("links to");
});

test("a link a save replaced between its lstat and its readlink is read as what is there now", async () => {
  // A save renames a regular file over the link: its readlink then answers EINVAL, and the read
  // goes on with the file the save left, in the login's own directory.
  const stub = broker();
  const path = loginPath();
  await mkdir(dirname(path), { recursive: true, mode: 0o700 });
  const target = join(dirname(path), "saved.json");
  await writeDeviceLoginFile(target, stub.url);
  await fs.symlink(target, path);
  let reads = 0;
  const realReadlink = fs.readlink;
  const readlinks = spyOn(fs, "readlink").mockImplementation((async (
    ...args: Parameters<typeof fs.readlink>
  ) => {
    reads += 1;
    if (reads === 1) throw Object.assign(new Error("not a link"), { code: "EINVAL" });
    return await realReadlink(...args);
  }) as typeof fs.readlink);
  try {
    expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
  } finally {
    readlinks.mockRestore();
  }
  // Control: the read did reach the link's readlink.
  expect(reads).toBe(1);
});

test("a lookup of the login's path that fails as missing does not make a regular login missing", async () => {
  // Bun's realpath opens the path and then reads where that open went, so it answers ENOENT where
  // a save renames a new file over the path between the two. A regular file needs no lookup.
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const lookups = spyOn(fs, "realpath").mockImplementation((async () => {
    throw Object.assign(new Error("not there"), { code: "ENOENT" });
  }) as unknown as typeof fs.realpath);
  try {
    expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
  } finally {
    lookups.mockRestore();
  }
});

/** Run by another process: saves the login at `SAVED_LOGIN` over and over, as a save does, by a
 *  rename of a whole new file over it, once it has said so on its standard output. */
const SAVER = `
const { readFileSync, renameSync, writeFileSync, writeSync } = require("node:fs");
const path = process.env.SAVED_LOGIN;
const login = readFileSync(path);
writeSync(1, "saving\\n");
for (;;) {
  writeFileSync(path + ".saving", login, { mode: 0o600 });
  renameSync(path + ".saving", path);
}
`;

test("a login read while another process saves it over and over is never refused", async () => {
  // Each read finds the file before the rename or the one after it, never no file. The row reads
  // until it has seen the saver's renames SAVES_SEEN times (a read after which the path is another
  // file than before it), so it races the same number of saves wherever it runs: where a read
  // costs more, each read sees a save, and it makes fewer reads. No clock decides how many.
  const SAVES_SEEN = 500;
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const saver = Bun.spawn([process.execPath, "-e", SAVER], {
    env: { ...process.env, SAVED_LOGIN: path },
    stdout: "pipe",
    stderr: "inherit",
  });
  const refusals: string[] = [];
  let saves = 0;
  try {
    const said = saver.stdout.getReader();
    let heard = "";
    while (!heard.includes("saving")) {
      const { value, done } = await said.read();
      if (done) throw new Error(`the saver ended before it saved: ${heard}`);
      heard += new TextDecoder().decode(value);
    }
    let before = (await stat(path)).ino;
    while (saves < SAVES_SEEN) {
      if (saver.exitCode !== null) throw new Error(`the saver ended: exit ${saver.exitCode}`);
      await readDeviceLogin(path).catch((error: unknown) => {
        refusals.push(error instanceof Error ? error.message : String(error));
      });
      const after = (await stat(path)).ino;
      if (after !== before) saves += 1;
      before = after;
    }
  } finally {
    saver.kill();
    await saver.exited;
  }
  // Control: the reads saw the saver's renames, as many times as the row asks.
  expect(saves).toBe(SAVES_SEEN);
  expect(refusals).toEqual([]);
}, 120_000);

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
