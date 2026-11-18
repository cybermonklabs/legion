import { afterAll, beforeAll, expect, test } from "bun:test"
import { writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { bunCacheOff } from "@legion/test-fixtures/bun-child-environment"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"

/** The idempotency key the shared transport mints for a send that names none. */
const MINTED_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

/** The sentence of the listener's 429 for a session past its sending limit
 *  (`senderRateLimitedAnswer`, packages/envoy/cmd/listener/sender_limit.go). */
const RATE_LIMITED_SENTENCE =
  "Your sending is rate limited. This message was not sent. This listener lets one session send " +
  "4 messages a minute and 2 at once. You can send it again after 15 seconds."

type SendBody = {
  readonly target_session: string
  readonly message: string
  readonly idempotency_key: string
}

const requests: { path: string; authorization: string | null; body: unknown }[] = []
// Whether the listener refuses every send for its sender's rate, as it does past the limit.
let rateLimited = false
const server = Bun.serve({
  port: 0,
  fetch: async (request) => {
    const body = (await request.json()) as SendBody
    requests.push({
      path: new URL(request.url).pathname,
      authorization: request.headers.get("authorization"),
      body,
    })
    if (rateLimited) {
      return Response.json(
        {
          error: RATE_LIMITED_SENTENCE,
          reason: "sender_rate_limited",
          retry_after_seconds: 15,
          messages_per_minute: 4,
          burst: 2,
        },
        { status: 429, headers: { "Retry-After": "15" } },
      )
    }
    // The listener's answer to a stored send: the envelope it stored, and the recipient.
    return Response.json({
      event_id: "evt_sent",
      source: "agent",
      source_event_id: "agent.evt_sent",
      topic: `notifications.agent.${body.target_session}`,
      dedupe_key: `agent.${body.target_session}.${body.idempotency_key}`,
      issued_at: 1,
      payload_summary: body.message,
      trace_id: "trace-sent",
      recipient: body.target_session,
    })
  },
})
const scratch = temporaryDirectory("claude-envoy-send")
const configFile = join(scratch, "envoy.json")
beforeAll(() => {
  writeFileSync(
    configFile,
    JSON.stringify({
      envoy: { enabled: true, url: `http://127.0.0.1:${server.port}`, token: "listener-bearer" },
    }),
  )
})
afterAll(() => {
  server.stop(true)
})

/**
 * Runs `envoy-send` with `words` in `scratch`, with `env`, PATH and the Bun cache key that turns
 * the child's transpiler cache off (`bunCacheOff`) as its whole environment.
 */
async function envoySend(
  words: readonly string[],
  env: Readonly<Record<string, string>>,
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const send = Bun.spawn(["bun", resolve(import.meta.dir, "../bin/envoy-send.ts"), ...words], {
    cwd: scratch,
    env: { ...bunCacheOff, PATH: process.env["PATH"] ?? "", HOME: scratch, ...env },
    stdout: "pipe",
    stderr: "pipe",
  })
  const [exitCode, stdout, stderr] = await Promise.all([
    send.exited,
    new Response(send.stdout).text(),
    new Response(send.stderr).text(),
  ])
  return { exitCode, stdout, stderr }
}

test("envoy-send sends to the listener and with the bearer its envoy.json names", async () => {
  requests.length = 0
  const sent = await envoySend(["ses_receiver", "file", "send"], { ENVOY_CONFIG_FILE: configFile })
  expect(sent.exitCode).toBe(0)
  expect(sent.stdout).toBe("Sent Envoy message to ses_receiver.\n")
  // A shell that names no session sends no source_session.
  expect(requests).toEqual([
    {
      path: "/v1/messages/send",
      authorization: "Bearer listener-bearer",
      body: {
        source: "agent",
        target_session: "ses_receiver",
        message: "file send",
        idempotency_key: expect.stringMatching(MINTED_KEY),
      },
    },
  ])
})

test("envoy-send whose sender is rate limited sends its session as source_session, prints the listener's sentence and exits 1", async () => {
  requests.length = 0
  rateLimited = true
  try {
    const sent = await envoySend(["ses_receiver", "limited", "send"], {
      ENVOY_CONFIG_FILE: configFile,
      ENVOY_SESSION_ID: "ses_sender",
    })
    expect(sent.exitCode).toBe(1)
    expect(sent.stderr).toBe(
      `Claude Envoy send failed: ${RATE_LIMITED_SENTENCE} (reason: sender_rate_limited)\n`,
    )
    // One request: a refusal is not sent again.
    expect(requests).toEqual([
      {
        path: "/v1/messages/send",
        authorization: "Bearer listener-bearer",
        body: {
          source: "agent",
          source_session: "ses_sender",
          target_session: "ses_receiver",
          message: "limited send",
          idempotency_key: expect.stringMatching(MINTED_KEY),
        },
      },
    ])
  } finally {
    rateLimited = false
  }
})

test("envoy-send with no Envoy configured refuses, saying why, and sends nothing", async () => {
  requests.length = 0
  const missing = join(scratch, "absent", "envoy.json")
  const sent = await envoySend(["ses_receiver", "nothing"], { ENVOY_CONFIG_FILE: missing })
  expect(sent.exitCode).toBe(1)
  expect(sent.stderr).toBe(
    `Claude Envoy send failed: Envoy is off: no envoy.json at ${missing}: Envoy is off\n`,
  )
  expect(requests).toEqual([])
})
