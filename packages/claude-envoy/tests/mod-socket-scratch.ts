import { mkdtemp } from "node:fs/promises"
import { join } from "node:path"
import { socketRoot } from "@legion/test-fixtures/socket-root"
import { resolveModSocketPath } from "../hooks/mod-protocol"

/**
 * Makes a fresh directory, as `mkdtemp(join(root, prefix))` does, that a session's mod socket fits
 * under when a test hands it to the server and the mod as `XDG_RUNTIME_DIR`, and returns its path.
 * The caller removes it, as it would a directory of `mkdtemp`'s.
 *
 * `root` is `tmpdir()`, as for every other scratch directory, unless the socket path under a
 * directory made there would pass `MOD_SOCKET_PATH_MAX_BYTES`, which `resolveModSocketPath`
 * refuses: then it is `/tmp`, and the call says so on stderr (`socketRoot`). A prefix too long for
 * either fails the call, naming the paths it measured.
 */
export async function modSocketScratch(prefix: string): Promise<string> {
  // The socket's name is a fixed-length hash of the session id, so any id measures every one.
  const root = socketRoot(
    "modSocketScratch",
    (root) =>
      resolveModSocketPath("scratch", { XDG_RUNTIME_DIR: join(root, `${prefix}XXXXXX`) }).refusal,
  )
  return mkdtemp(join(root, prefix))
}
