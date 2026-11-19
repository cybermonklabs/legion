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
import { afterEach, describe, expect, test } from "bun:test"
import { randomBytes } from "node:crypto"
import { writeFileSync } from "node:fs"
import * as path from "node:path"
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories"

const servers: { stop: (closeActive?: boolean) => void }[] = []
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true)
})
const tempDir = temporaryDirectories("envoy-send-url")
const command = path.join(import.meta.dir, "..", "bin", "envoy-send.ts")

/** Runs envoy-send against `url` as the listener the session's envoy.json names. */
async function send(url: string): Promise<{ exitCode: number; stderr: string; stdout: string }> {
  const home = tempDir()
  const config = path.join(home, "envoy.json")
  writeFileSync(
    config,
    JSON.stringify({ envoy: { enabled: true, url }, dispatch: { enabled: false } }),
  )
  const child = Bun.spawn(["bun", command, "ses_target", "hello"], {
    env: { PATH: process.env["PATH"] ?? "", HOME: home, TMPDIR: home, ENVOY_CONFIG_FILE: config },
    stdout: "pipe",
    stderr: "pipe",
  })
  const [stdout, stderr, exitCode] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ])
  return { exitCode, stderr, stdout }
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
    const { exitCode, stderr } = await send(`http://${user}:${password}@127.0.0.1:${server.port}`)
    // The row can fail for the leak: the send reached the listener and failed.
    expect(seen.length).toBeGreaterThan(0)
    expect(exitCode).toBe(1)
    expect(stderr).toContain("Claude Envoy send failed")
    expect(stderr).not.toContain(user)
    expect(stderr).not.toContain(password)
  }, 20_000)

  test("a password with an `@` in it", async () => {
    const secret = `s${randomBytes(8).toString("hex")}`
    const { exitCode, stderr } = await send(`http://user:p@ss${secret}`)
    expect(exitCode).toBe(1)
    expect(stderr).toContain("Claude Envoy send failed")
    expect(stderr).not.toContain(secret)
  }, 20_000)
})
