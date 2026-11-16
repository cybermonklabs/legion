import { tmpdir } from "node:os";

/** Where a directory goes when one under `tmpdir()` leaves its socket no room: the shortest
 *  temporary root a machine has. */
const FALLBACK_ROOT = "/tmp";

/** The fallback notes printed so far: each once per process, not once per directory, since a
 *  file can make a directory per row. */
const noted = new Set<string>();

/**
 * The root to make a temporary directory in when a unix socket goes under it: a socket's path holds
 * at most 107 bytes (docs/testing.md, "Unix sockets in Go tests"), and a long `TMPDIR`, such as a
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

/** Why `path`, a unix socket's, would not fit the 107 bytes a socket path holds, or `undefined`
 *  when it fits: the `refusal` of {@link socketRoot} for a caller with no bound of its own. */
export function socketPathRefusal(path: string): string | undefined {
  const bytes = Buffer.byteLength(path);
  return bytes > 107
    ? `${path} is ${bytes} bytes, past the 107 a unix socket path holds`
    : undefined;
}
