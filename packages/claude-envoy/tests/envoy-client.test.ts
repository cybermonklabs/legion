import { afterAll, expect, test } from "bun:test"
import { writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { sendEnvoyMessage } from "../src/envoy-client"

const requests: { path: string; authorization: string | null; body: string }[] = []
const server = Bun.serve({
  port: 0,
  fetch: async (request) => {
    requests.push({
      path: new URL(request.url).pathname,
      authorization: request.headers.get("authorization"),
      body: await request.text(),
    })
    return new Response(null, { status: 202 })
  },
})
const scratch = temporaryDirectory("claude-envoy-send")
afterAll(() => {
  server.stop(true)
})

test("sends a direct message through Envoy's HTTP API, with no bearer for a listener that asks for none", async () => {
  requests.length = 0
  await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential: undefined,
    targetSession: "ses_receiver",
    message: "adapter send proof",
  })
  expect(requests).toEqual([
    {
      path: "/v1/messages/send",
      authorization: null,
      body: JSON.stringify({ target_session: "ses_receiver", message: "adapter send proof" }),
    },
  ])
})

/** What `call` settled with by the next turn of the event loop, or "pending": the test's bound. */
async function outcome(call: Promise<unknown>): Promise<unknown> {
  const bound = Promise.withResolvers<"pending">()
  setImmediate(() => bound.resolve("pending"))
  return await Promise.race([
    call.then(
      (value) => value,
      (error: unknown) => error,
    ),
    bound.promise,
  ])
}

test("a send hands the credential its deadline: waiting on a pod's missing registration, it fails at that deadline naming the session, and sends once the registration lands", async () => {
  requests.length = 0
  let registered = false
  const given: (AbortSignal | undefined)[] = []
  // A broker credential while the pod's registration is missing: it answers nothing until its
  // caller's deadline ends, then says the session's registration is pending.
  const credential = {
    scheme: "DPoP" as const,
    headers: (_method: string, _url: string, _session?: string, signal?: AbortSignal) => {
      given.push(signal)
      if (registered) return { Authorization: "DPoP registered-token" }
      const { promise, reject } = Promise.withResolvers<Readonly<Record<string, string>>>()
      signal?.addEventListener(
        "abort",
        () => reject(new Error("registration pending: implementer-g1")),
        {
          once: true,
        },
      )
      return promise
    },
    stale: () => {},
    secret: () => "",
  }
  const options = {
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential,
    targetSession: "ses_receiver",
    message: "sent once registered",
  }
  // The send's deadline is a signal the test ends, standing in for the call's own timer.
  const deadline = new AbortController()
  const send = sendEnvoyMessage({ ...options, signal: deadline.signal })
  await new Promise((resolve) => setImmediate(resolve))
  expect(given[0], "the send handed its credential no deadline").toBeInstanceOf(AbortSignal)
  // The control: while the deadline is live the send is still waiting on the credential.
  expect(await outcome(send)).toBe("pending")
  deadline.abort()
  const refused = await outcome(send)
  expect(refused, "the send did not return at its deadline").toBeInstanceOf(Error)
  expect((refused as Error).message).toContain("implementer-g1")
  expect(requests).toEqual([])
  registered = true
  await sendEnvoyMessage({ ...options, signal: new AbortController().signal })
  expect(requests.map((one) => one.authorization)).toEqual(["DPoP registered-token"])
})

test("envoy-send sends to the listener and with the bearer its envoy.json names", async () => {
  requests.length = 0
  const file = join(scratch, "envoy.json")
  writeFileSync(
    file,
    JSON.stringify({
      envoy: { enabled: true, url: `http://127.0.0.1:${server.port}`, token: "listener-bearer" },
    }),
  )
  const send = Bun.spawn(
    ["bun", resolve(import.meta.dir, "../bin/envoy-send.ts"), "ses_receiver", "file", "send"],
    {
      cwd: scratch,
      env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: file },
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  expect(await send.exited).toBe(0)
  expect(await new Response(send.stdout).text()).toBe("Sent Envoy message to ses_receiver.\n")
  expect(requests).toEqual([
    {
      path: "/v1/messages/send",
      authorization: "Bearer listener-bearer",
      body: JSON.stringify({ target_session: "ses_receiver", message: "file send" }),
    },
  ])
})

test("envoy-send with no Envoy configured refuses, saying why, and sends nothing", async () => {
  requests.length = 0
  const missing = join(scratch, "absent", "envoy.json")
  const send = Bun.spawn(
    ["bun", resolve(import.meta.dir, "../bin/envoy-send.ts"), "ses_receiver", "nothing"],
    {
      cwd: scratch,
      env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: missing },
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  expect(await send.exited).toBe(1)
  expect(await new Response(send.stderr).text()).toBe(
    `Claude Envoy send failed: Envoy is off: no envoy.json at ${missing}: Envoy is off\n`,
  )
  expect(requests).toEqual([])
})
