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

/**
 * The socket path `sessionId` gets under `env`, as the server and the mod resolve it. A session
 * with none fails the test, and the error says why: the `refusal` `resolveModSocketPath` gives,
 * such as the path that went past `MOD_SOCKET_PATH_MAX_BYTES`. A bare "names no mod socket" left a
 * long `TMPDIR` to be guessed at.
 */
export function sessionModSocket(
  sessionId: string,
  env: Readonly<Record<string, string | undefined>>,
): string {
  const resolution = resolveModSocketPath(sessionId, env)
  if (resolution.path === undefined) {
    throw new Error(`the session names no mod socket: ${resolution.refusal}`)
  }
  return resolution.path
}
