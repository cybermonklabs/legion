import { tmpdir } from "node:os";

/** Where a directory goes when one under `tmpdir()` leaves its socket no room: the shortest
 *  temporary root a machine has. */
const FALLBACK_ROOT = "/tmp";

/** The fallback notes printed so far: each once per process, not once per directory, since a
 *  file can make a directory per row. */
const noted = new Set<string>();

/**
 * The root to make a temporary directory in when a unix socket goes under it: a socket's path holds
 * at most 107 bytes on Linux and 103 on macOS (`socketPathRefusal`), and a long `TMPDIR`, such as a
 * dev box's data disk or a macOS `/var/folders` path, passes it, so a test would fail on where the
 * machine keeps its temporary files rather than on what it tests.
 *
 * `refusal(root)` answers why the caller's longest socket path under a directory made in `root`
 * would not fit, or `undefined` when it fits; the caller measures that path itself, its directory's
 * name as `mkdtemp` makes it (six `X` for the random part) included. The answer is `tmpdir()` when
 * that fits, and otherwise `/tmp`, for the socket path's length alone, as the Go modules'
 * `internal/testtmp` does; `what` names the caller in the note that says so on stderr, once per
 * process. It throws, naming both refusals, when neither fits.
 *
 * It registers no test hook, so a harness that runs outside `bun test` (the Dispatch e2e pool,
 * under Playwright) takes it too.
 */
export function socketRoot(what: string, refusal: (root: string) => string | undefined): string {
  const refusals: string[] = [];
  for (const root of new Set([tmpdir(), FALLBACK_ROOT])) {
    const refused = refusal(root);
    if (refused !== undefined) {
      refusals.push(refused);
      continue;
    }
    const note = `${what}: ${refusals.join("; ")}; falling back to ${root}`;
    if (refusals.length > 0 && !noted.has(note)) {
      noted.add(note);
      console.error(note);
    }
    return root;
  }
  throw new Error(`${what}: no temporary root leaves room for its socket: ${refusals.join("; ")}`);
}

/** Why `path`, a unix socket's, would not fit the bytes a socket path holds on `platform` (this
 *  process's unless a test names another), or `undefined` when it fits: the `refusal` of
 *  {@link socketRoot} for a caller with no bound of its own. */
export function socketPathRefusal(
  path: string,
  platform: NodeJS.Platform = process.platform
): string | undefined {
  // The longest path is the system's `sockaddr_un.sun_path` less the terminating zero, which
  // every program a test binds or dials with keeps inside it: Postgres refuses a path whose length
  // reaches `sizeof(sun_path)`, and Go and tmux do the same. Linux's `sun_path` holds 108 bytes
  // (`UNIX_PATH_MAX` in the kernel's include/uapi/linux/un.h, and glibc's sys/un.h) and macOS's
  // 104 (xnu's bsd/sys/un.h). Any other system gets macOS's 103, the smaller of the two.
  const limit = platform === "linux" ? 107 : 103;
  const bytes = Buffer.byteLength(path);
  return bytes > limit
    ? `${path} is ${bytes} bytes, past the ${limit} a unix socket path holds on ${platform}`
    : undefined;
}
