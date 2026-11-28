// A failing test for #4140 at dc8dc9ea7a, found by the acceptance (AcceptE2-2): `envoy-send`'s
// failure line names the listener URL whole, user info included. ky's HTTPError message is
// "Request failed with status code 401 Unauthorized: POST <the URL>", and a URL whose password
// holds an `@` fails as "getaddrinfo EAI_AGAIN <the rest of the password>" (the text after the
// last `@` read as the host). bin/envoy-send.ts prints either as it came
// (`Claude Envoy send failed: ${messageFor(error)}`), while the other clients name a listener URL
// through urlForText. On the real stack (the acceptance's leak matrix, its envoy-send column):
// d-listener-url-userinfo, d-listener-env-userinfo, i-basic-listener-echo and
// d-listener-url-at-in-password. Put it in packages/claude-envoy/tests/ and run
// `bun test tests/envoy-send-url.test.ts` in packages/claude-envoy.
import { afterEach, beforeEach, describe, expect, setDefaultTimeout, test } from "bun:test"
import { randomBytes } from "node:crypto"
import { writeFileSync } from "node:fs"
import * as path from "node:path"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"
import { collected, exitBeforeTheGuard, ROW_GUARD_MS, rowStarted } from "./row-deadline"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"
import { unresolvedNames } from "./unresolved-names"

const servers: { stop: (closeActive?: boolean) => void }[] = []
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true)
})
const tempDir = temporaryDirectories("envoy-send-url")
const command = path.join(import.meta.dir, "..", "bin", "envoy-send.ts")

// Each row waits for the send to end, and for nothing else; a send that never ends fails the row
// by name before the hang guard this file sets as its rows' default (`ROW_GUARD_MS`, 120 s, `./row-deadline.ts`). A fixed 20 s
// decided the 401 row before: its child wrote Bun's transpiler cache into its empty HOME first
// (`./transpiler-cache.ts`), and at a load of about 900 it had not reached the listener in 20 s.
beforeEach(rowStarted)
setDefaultTimeout(ROW_GUARD_MS)

/** Runs envoy-send against `url` as the listener the session's envoy.json names, with `preload`
 *  given to Bun first where set. `ended` is how it ended: its exit code, or the signal that ended
 *  it. */
async function send(
  url: string,
  preload?: string,
): Promise<{ ended: string; exitCode: number; stderr: string; stdout: string }> {
  const home = tempDir()
  const config = path.join(home, "envoy.json")
  writeFileSync(
    config,
    JSON.stringify({ envoy: { enabled: true, url }, dispatch: { enabled: false } }),
  )
  const preloading = preload === undefined ? [] : ["--preload", preload]
  const child = Bun.spawn(["bun", ...preloading, command, "ses_target", "hello"], {
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: home,
      TMPDIR: home,
      ENVOY_CONFIG_FILE: config,
      ...NO_TRANSPILER_CACHE,
    },
    stdout: "pipe",
    stderr: "pipe",
  })
  const stdout = collected(child.stdout)
  const stderr = collected(child.stderr)
  const exitCode = await exitBeforeTheGuard(
    child,
    "envoy-send",
    () =>
      `it wrote ${JSON.stringify(stdout.text())} to stdout, ${JSON.stringify(stderr.text())} to stderr`,
  )
  const ended = child.signalCode === null ? `exit ${exitCode}` : `signal ${child.signalCode}`
  return { ended, exitCode, stderr: await stderr.ended, stdout: await stdout.ended }
}

describe("envoy-send's failure line names no part of the listener URL's user info", () => {
  test("a listener that answers 401", async () => {
    const user = `u${randomBytes(6).toString("hex")}`
    const password = `p${randomBytes(8).toString("hex")}`
    const seen: string[] = []
    const server = Bun.serve({
      hostname: "127.0.0.1",
      port: 0,
      fetch: (request) => {
        seen.push(request.headers.get("authorization") ?? "")
        return new Response("unauthorized", { status: 401 })
      },
    })
    servers.push(server)
    const { ended, exitCode, stderr } = await send(
      `http://${user}:${password}@127.0.0.1:${server.port}`,
    )
    // The row can fail for the leak: the send reached the listener and failed.
    expect(
      seen.length,
      `the send ended (${ended}) without reaching the listener: ${stderr}`,
    ).toBeGreaterThan(0)
    expect(exitCode).toBe(1)
    expect(stderr).toContain("Claude Envoy send failed")
    expect(stderr).not.toContain(user)
    expect(stderr).not.toContain(password)
  })

  // The host the URL parser reads is the password's tail, under `.invalid`; the send's fetch of it
  // is answered by a stand-in that fails as Bun's resolver does (`unresolved-names.ts`), so the row
  // asks no DNS server and does not wait on its answer.
  test("a password with an `@` in it", async () => {
    const secret = `s${randomBytes(8).toString("hex")}`
    const names = await unresolvedNames(tempDir())
    const { exitCode, stderr } = await send(`http://user:p@ss${secret}.invalid`, names.preload)
    // The send's error named the secret, so the checks below can fail. The send tries a request
    // that reached nothing again, so the stand-in may be asked more than once, and only this host.
    const asked = await names.asked()
    expect(asked.length).toBeGreaterThan(0)
    expect(asked.filter((name) => name !== `ss${secret}.invalid`)).toEqual([])
    expect(exitCode).toBe(1)
    expect(stderr).toContain("Claude Envoy send failed")
    expect(stderr).not.toContain(secret)
  })
})
