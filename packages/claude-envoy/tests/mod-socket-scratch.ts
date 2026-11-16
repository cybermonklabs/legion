import { mkdtemp } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { MOD_SOCKET_PATH_MAX_BYTES, resolveModSocketPath } from "../hooks/mod-protocol"

/** Where a scratch directory goes when one under `tmpdir()` leaves no room for the socket. */
const FALLBACK_ROOT = "/tmp"

/** What `mkdtemp` adds to a prefix: six characters. */
const MKDTEMP_SUFFIX = "XXXXXX"

/** The fallback notes printed so far. Each is printed once per `bun test` process rather than
 *  once per directory, since a file such as `mod-link.test.ts` makes a directory per row. */
const noted = new Set<string>()

/**
 * Makes a fresh directory, as `mkdtemp(join(root, prefix))` does, that a session's mod socket fits
 * under when a test hands it to the server and the mod as `XDG_RUNTIME_DIR`, and returns its path.
 * The caller removes it, as it would a directory of `mkdtemp`'s.
 *
 * `root` is `tmpdir()`, as for every other scratch directory, unless the socket path under a
 * directory made there would pass `MOD_SOCKET_PATH_MAX_BYTES`. A long `TMPDIR`, such as a dev
 * box's data disk or a macOS `/var/folders` path, passes it, and `resolveModSocketPath` refuses
 * such a base, so the test would fail on where the machine keeps its temporary files rather than
 * on what it tests. Then `root` is `/tmp`, for the socket path's length alone, as the Go modules'
 * `internal/testtmp` does (docs/testing.md, "Unix sockets in Go tests"), and the call says so on
 * stderr. A prefix too long for either fails the call, naming the paths it measured.
 */
export async function modSocketScratch(prefix: string): Promise<string> {
  const refusals: string[] = []
  for (const root of new Set([tmpdir(), FALLBACK_ROOT])) {
    const longest = join(root, `${prefix}${MKDTEMP_SUFFIX}`)
    // The socket's name is a fixed-length hash of the session id, so any id measures every one.
    const resolution = resolveModSocketPath("scratch", { XDG_RUNTIME_DIR: longest })
    if (resolution.path === undefined) {
      refusals.push(resolution.refusal)
      continue
    }
    const note = `modSocketScratch: ${refusals.join("; ")}; falling back to ${root}`
    if (refusals.length > 0 && !noted.has(note)) {
      noted.add(note)
      console.error(note)
    }
    return mkdtemp(join(root, prefix))
  }
  throw new Error(
    `modSocketScratch: no root leaves room for a mod socket within ${MOD_SOCKET_PATH_MAX_BYTES} bytes under a ${JSON.stringify(prefix)} directory: ${refusals.join("; ")}`,
  )
}
