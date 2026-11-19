import { afterAll, expect, test } from "bun:test"
import { mkdirSync, writeFileSync } from "node:fs"
import { createServer, type Server } from "node:net"
import { join, resolve } from "node:path"
import { signedIn, stubIssuer, writeSignIn } from "@legion/broker-client/test/sign-ins"
import { SignInEndedError } from "@legion/broker-client/token-errors"
import { URL_NOT_PRINTED } from "@legion/contracts/log-text"
import { unshownSettingUrl } from "@legion/contracts/setting-url"
import type { RequestCredential } from "@legion/envoy-client/bearer"
import { EnvoySendUnconfirmedError } from "@legion/envoy-client/transport"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { REDIRECT_CODES, redirectServers } from "@legion/test-fixtures/redirect-servers"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import { sendEnvoyMessage } from "../src/envoy-client"

/** What one send reached the stand-in listener with. */
interface Sent {
  readonly path: string
  readonly authorization: string | null
  readonly body: Record<string, unknown>
}

/** The envelope a listener answers a stored send with. */
function stored(target: string): Record<string, unknown> {
  return {
    event_id: "evt-sent",
    source: "agent",
    source_event_id: "agent.ses_sender.evt-sent",
    topic: `notifications.agent.${target}`,
    dedupe_key: `agent.${target}.evt-sent`,
    issued_at: 1,
    payload_summary: "message",
    trace_id: "trace-sent",
    recipient: target,
    recipient_state: "live",
  }
}

const requests: Sent[] = []
/** What the stand-in listener answers each send, in order; past the end it stores the send. */
const answers: (() => Response)[] = []
const server = Bun.serve({
  port: 0,
  fetch: async (request) => {
    const body = JSON.parse(await request.text()) as Record<string, unknown>
    requests.push({
      path: new URL(request.url).pathname,
      authorization: request.headers.get("authorization"),
      body,
    })
    const answer = answers.shift()
    return answer?.() ?? Response.json(stored(String(body["target_session"])), { status: 202 })
  },
})
const scratch = temporaryDirectory("claude-envoy-send")
afterAll(() => {
  server.stop(true)
})

/** A lower-case UUID, the idempotency key the client mints for a send. */
const MINTED_KEY = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/

test("sends a direct message through Envoy's HTTP API under an idempotency key, with no bearer for a listener that asks for none", async () => {
  requests.length = 0
  await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential: undefined,
    targetSession: "ses_receiver",
    message: "adapter send proof",
  })
  expect(requests).toHaveLength(1)
  expect(requests[0]?.path).toBe("/v1/messages/send")
  expect(requests[0]?.authorization).toBeNull()
  expect(requests[0]?.body).toMatchObject({
    source: "agent",
    target_session: "ses_receiver",
    message: "adapter send proof",
  })
  // No source session was named, so the transport leaves source_session off the wire.
  expect(requests[0]?.body).not.toHaveProperty("source_session")
  expect(String(requests[0]?.body["idempotency_key"])).toMatch(MINTED_KEY)
})

// A send whose attempt the listener may have acted on and whose answer was lost is sent again
// under the same key, so the listener stores it once: before, envoy-send made one attempt with no
// key, so a send that landed and lost its answer failed, and running it again delivered it twice.
test("a send whose first attempt got no answer is sent again under the same key", async () => {
  requests.length = 0
  answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
  await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential: undefined,
    targetSession: "ses_receiver",
    message: "sent once",
  })
  expect(requests).toHaveLength(2)
  const keys = requests.map((one) => one.body["idempotency_key"])
  expect(keys[0]).toBe(keys[1])
  expect(String(keys[0])).toMatch(MINTED_KEY)
})

test("a send no attempt of which was answered says it may have arrived, naming the key that resends it safely", async () => {
  requests.length = 0
  for (let n = 0; n < 2; n++) {
    answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
  }
  const refused = await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential: undefined,
    targetSession: "ses_receiver",
    message: "maybe sent",
  }).catch((error: unknown) => error)
  expect(refused).toBeInstanceOf(EnvoySendUnconfirmedError)
  const unconfirmed = refused as EnvoySendUnconfirmedError
  expect(unconfirmed.resendSafe).toBe(true)
  expect(unconfirmed.idempotencyKey).toBe(String(requests[0]?.body["idempotency_key"]))
  expect(unconfirmed.message).toContain(`idempotency_key ${unconfirmed.idempotencyKey}`)
})

/** A credential that waits on the broker until its signal ends, then rejects with `words`, and
 *  answers with `token` once `ready()` says so. `given` keeps each signal it was handed. */
function waitingCredential(words: string, ready: () => string | undefined = () => undefined) {
  const given: (AbortSignal | undefined)[] = []
  return {
    given,
    credential: {
      scheme: "DPoP" as const,
      headers: (_method: string, _url: string, _session?: string, signal?: AbortSignal) => {
        given.push(signal)
        const token = ready()
        if (token !== undefined) return { Authorization: `DPoP ${token}` }
        const { promise, reject } = Promise.withResolvers<Readonly<Record<string, string>>>()
        signal?.addEventListener("abort", () => reject(new Error(words)), { once: true })
        return promise
      },
      stale: () => {},
      secret: () => "",
    },
  }
}

test("a send whose credential waits on a pod's missing registration goes without it after its share, and carries the token once the registration lands", async () => {
  requests.length = 0
  let registered = false
  const { credential, given } = waitingCredential("registration pending: implementer-g1", () =>
    registered ? "registered-token" : undefined,
  )
  const options = {
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential,
    targetSession: "ses_receiver",
    message: "sent while the registration is pending",
  }
  const started = Date.now()
  await sendEnvoyMessage(options)
  // The credential waited the attempt less the request's reserve, not the send's whole time.
  expect(given[0], "the send handed its credential no deadline").toBeInstanceOf(AbortSignal)
  expect(Date.now() - started).toBeLessThan(5_000)
  expect(requests.map((one) => one.authorization)).toEqual([null])
  registered = true
  await sendEnvoyMessage(options)
  expect(requests.map((one) => one.authorization)).toEqual([null, "DPoP registered-token"])
}, 20_000)

test("a send whose credential's trade never answers goes without it, and a listener that asks for none takes it", async () => {
  // The credential waits the attempt's 5 s less the request's 1 s reserve (`credentialDeadline`),
  // then the send goes without it, as envoy-client's transport does.
  requests.length = 0
  const started = Date.now()
  await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential: waitingCredential("the secrets broker has not yet answered the trade").credential,
    targetSession: "ses_receiver",
    message: "sent without a credential",
  })
  expect(Date.now() - started).toBeLessThan(5_000)
  expect(requests.map((one) => one.authorization)).toEqual([null])
}, 20_000)

// A listener that wants a credential refuses a request without one at its auth gate, before any
// route runs (`apiauth.go`), so nothing was stored: the error names the 401 and why the
// credential could not be minted, and never says the message may have arrived.
test("a listener that wants a credential answers 401, and the send's error names both halves and no resend", async () => {
  const wanting = Bun.serve({
    port: 0,
    fetch: () => Response.json({ error: "this listener wants a credential" }, { status: 401 }),
  })
  try {
    const refused = await sendEnvoyMessage({
      baseUrl: `http://127.0.0.1:${wanting.port}`,
      credential: waitingCredential("the secrets broker has not yet answered the trade").credential,
      targetSession: "ses_receiver",
      message: "refused",
    }).catch((error: unknown) => error)
    expect(refused).not.toBeInstanceOf(EnvoySendUnconfirmedError)
    expect((refused as Error).message).toBe(
      `POST http://127.0.0.1:${wanting.port}/v1/messages/send answered 401, and this machine's Envoy credential could not be minted: the secrets broker has not yet answered the trade`,
    )
  } finally {
    wanting.stop(true)
  }
}, 30_000)

test("envoy-send sends to the listener and with the bearer its envoy.json names", async () => {
  requests.length = 0
  const listenerToken = fixtureSecret("listener")
  const file = join(scratch, "envoy.json")
  writeFileSync(
    file,
    JSON.stringify({
      envoy: { enabled: true, url: `http://127.0.0.1:${server.port}`, token: listenerToken },
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
  expect(requests).toHaveLength(1)
  expect(requests[0]?.path).toBe("/v1/messages/send")
  expect(requests[0]?.authorization).toBe(`Bearer ${listenerToken}`)
  expect(requests[0]?.body).toMatchObject({ target_session: "ses_receiver", message: "file send" })
  // The shell names no session, so the transport leaves source_session off the wire.
  expect(requests[0]?.body).not.toHaveProperty("source_session")
  expect(String(requests[0]?.body["idempotency_key"])).toMatch(MINTED_KEY)
})

/** The envoy.json `runEnvoySend` writes, naming the stand-in listener and `token` as its bearer. */
function envoyConfig(token: string, port = server.port): string {
  const file = join(scratch, `envoy-${port}.json`)
  writeFileSync(
    file,
    JSON.stringify({ envoy: { enabled: true, url: `http://127.0.0.1:${port}`, token } }),
  )
  return file
}

/**
 * Runs `envoy-send` as a person does, against the stand-in listener, with a bearer file: with
 * `args`, or as `command` through bash. `env` replaces the environment it runs in, which is
 * otherwise PATH, HOME and the ENVOY_CONFIG_FILE naming `token`'s envoy.json.
 */
async function runEnvoySend(
  args: readonly string[],
  options: {
    readonly command?: string
    readonly env?: Record<string, string>
    readonly token?: string
  } = {},
) {
  const file = envoyConfig(options.token ?? fixtureSecret("listener"))
  const send = Bun.spawn(
    options.command === undefined
      ? ["bun", resolve(import.meta.dir, "../bin/envoy-send.ts"), ...args]
      : ["bash", "-c", options.command],
    {
      cwd: scratch,
      env: options.env ?? {
        PATH: process.env["PATH"] ?? "",
        HOME: scratch,
        ENVOY_CONFIG_FILE: file,
      },
      stdout: "pipe",
      stderr: "pipe",
    },
  )
  const [code, stdout, stderr] = await Promise.all([
    send.exited,
    new Response(send.stdout).text(),
    new Response(send.stderr).text(),
  ])
  return { code, stdout, stderr, file }
}

/** The ready-to-run resend an unconfirmed send printed, if it printed one. */
function printedResend(stderr: string): string | undefined {
  return /To send it again safely, run: (.+)\n/.exec(stderr)?.[1]
}

/** The ready-to-run resend an unconfirmed send printed; the row fails where it printed none. */
function mustResend(stderr: string): string {
  const resend = printedResend(stderr)
  if (resend === undefined) throw new Error(`no resend command was printed: ${stderr}`)
  return resend
}

/** A credential whose every `headers` answers `answer`'s headers, or throws what it throws. */
function credentialOf(answer: () => Readonly<Record<string, string>>): RequestCredential {
  return { scheme: "DPoP", headers: answer, secret: () => "" }
}

// M3 (the review of 2fa6fbf7cf): a send its credential could not be presented for goes without
// it, and a listener that asks for none stores it. The send says what it went without: the very
// error the credential threw for the attempt the listener stored, and nothing where that attempt
// carried the credential, a credential that failed an earlier attempt included.
test("a send says what credential it went without, and nothing where it carried one", async () => {
  const baseUrl = `http://127.0.0.1:${server.port}`
  const base = { baseUrl, targetSession: "ses_receiver", message: "sent" }
  const ended = new SignInEndedError("https://issuer.test", "person@example.test")
  requests.length = 0
  const without = await sendEnvoyMessage({
    ...base,
    credential: credentialOf(() => {
      throw ended
    }),
  })
  expect(without.withoutCredential).toBe(ended)
  expect(requests.map((one) => one.authorization)).toEqual([null])

  const token = fixtureSecret("token")
  const carried = await sendEnvoyMessage({
    ...base,
    credential: credentialOf(() => ({ Authorization: `DPoP ${token}` })),
  })
  expect(carried.withoutCredential).toBeUndefined()

  // The first attempt's credential rejects and the listener answers 503; the second carries it.
  answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
  let calls = 0
  const later = await sendEnvoyMessage({
    ...base,
    credential: {
      scheme: "DPoP",
      headers: async () => {
        calls += 1
        if (calls === 1) throw ended
        return { Authorization: `DPoP ${token}` }
      },
      secret: () => "",
    },
  })
  expect(later.withoutCredential).toBeUndefined()
  expect(requests.map((one) => one.authorization)).toEqual([
    null,
    `DPoP ${token}`,
    null,
    `DPoP ${token}`,
  ])
  expect(
    (await sendEnvoyMessage({ ...base, credential: undefined })).withoutCredential,
  ).toBeUndefined()
})

/** The person whose sign-in `signedInEnvironment` keeps, and the sign-in client it names. */
const PERSON = "ada@example.com"
const SIGNIN_CLIENT = "example-device-client"

/**
 * The environment of a machine `PERSON` signed in on, as `agent-secrets login` leaves it, whose
 * envoy.json names the stand-in listener and no bearer, so the send's credential is the person's
 * broker trade. The issuer at `issuerUrl` renews the sign-in; `brokerUrl` is the secrets broker.
 */
async function signedInEnvironment(
  issuerUrl: string,
  brokerUrl: string,
): Promise<Record<string, string>> {
  const home = join(scratch, `signed-in-${crypto.randomUUID()}`)
  mkdirSync(home, { recursive: true })
  const signins = join(home, "signins")
  await writeSignIn(signins, await signedIn(PERSON, { client_id: SIGNIN_CLIENT }))
  const clientSecret = join(home, "client-secret")
  writeFileSync(clientSecret, `secret-${crypto.randomUUID()}\n`, { mode: 0o600 })
  const projected = join(home, "token")
  writeFileSync(projected, `${fixtureSecret("proj")}\n`, { mode: 0o600 })
  const config = join(home, "envoy.json")
  writeFileSync(
    config,
    JSON.stringify({ envoy: { enabled: true, url: `http://127.0.0.1:${server.port}` } }),
  )
  return {
    PATH: process.env["PATH"] ?? "",
    HOME: home,
    ENVOY_CONFIG_FILE: config,
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: projected,
    AGENT_SECRETS_SIGNIN_DIR: signins,
    AGENT_SECRETS_SIGNIN_ISSUER: issuerUrl,
    AGENT_SECRETS_SIGNIN_CLIENT_ID: SIGNIN_CLIENT,
    AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE: clientSecret,
  }
}

// M3 (the review of 2fa6fbf7cf): a revoked sign-in is a final refusal of the send's credential.
// A listener that asks for no credential stores the send made without it, so envoy-send says it
// sent and exits 0, and names on one stderr line that it went without the credential, why, that
// the refusal is final, and its remedy as the refusal words it. Before, it said only that it sent.
test("envoy-send whose sign-in the issuer ended says it sent, and on one stderr line why it went without the credential", async () => {
  requests.length = 0
  const issuer = await stubIssuer({
    clientId: SIGNIN_CLIENT,
    person: PERSON,
    answer: () => ({
      status: 400,
      body: { error: "invalid_grant", error_description: "Token has been expired or revoked." },
    }),
  })
  let traded = 0
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      traded += 1
      return new Response("this row trades nothing", { status: 500 })
    },
  })
  try {
    const env = await signedInEnvironment(issuer.url, `http://127.0.0.1:${broker.port}`)
    const sent = await runEnvoySend(["ses_receiver", "sent", "without"], { env })
    expect(sent.stdout).toBe("Sent Envoy message to ses_receiver.\n")
    expect(sent.stderr).toBe(
      "Claude Envoy sent the message without this machine's Envoy credential, which this listener does not ask for; " +
        "the credential was refused for good, so no retry mints it, and a listener that asks for it refuses every send from this machine until the refusal is cured: " +
        `${issuer.url} no longer honours ${PERSON}'s sign-in on this machine; run: agent-secrets login\n`,
    )
    expect(sent.code).toBe(0)
    expect(requests.map((one) => one.authorization)).toEqual([null])
    expect(traded).toBe(0)
  } finally {
    issuer.stop()
    broker.stop(true)
  }
}, 30_000)

// A credential a later trade may still mint (the broker answers 503) is named as one that could
// not be minted, and not as refused for good. The token client's retry loop logs its own line of
// the trade it retries (`token-client.ts`), so this row reads the line envoy-send writes.
test("envoy-send whose broker is down says it sent, and on one stderr line that the credential could not be minted", async () => {
  requests.length = 0
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => new Response("the broker is away", { status: 503 }),
  })
  try {
    const env = await signedInEnvironment("http://127.0.0.1:9", `http://127.0.0.1:${broker.port}`)
    for (const name of Object.keys(env))
      if (name.startsWith("AGENT_SECRETS_SIGNIN_")) delete env[name]
    const sent = await runEnvoySend(["ses_receiver", "sent", "without"], { env })
    expect(sent.code).toBe(0)
    expect(sent.stdout).toBe("Sent Envoy message to ses_receiver.\n")
    const said = sent.stderr.split("\n").filter((line) => line.startsWith("Claude Envoy"))
    expect(said).toHaveLength(1)
    expect(said[0]).toStartWith(
      "Claude Envoy sent the message without this machine's Envoy credential, which this listener does not ask for; the credential could not be minted: ",
    )
    expect(said[0]).toContain("HTTP 503")
    expect(said[0]).toEndWith("and the trade is being retried in the background")
    expect(sent.stderr).not.toContain("refused for good")
    expect(requests.map((one) => one.authorization)).toEqual([null])
  } finally {
    broker.stop(true)
  }
}, 30_000)

// The control of the two rows above: a send that carried its credential prints only that it sent.
test("envoy-send whose credential works prints only that it sent", async () => {
  requests.length = 0
  const token = fixtureSecret("listener")
  const sent = await runEnvoySend(["ses_receiver", "sent", "with", "it"], { token })
  expect(sent.code).toBe(0)
  expect(sent.stdout).toBe("Sent Envoy message to ses_receiver.\n")
  expect(sent.stderr).toBe("")
  expect(requests.map((one) => one.authorization)).toEqual([`Bearer ${token}`])
}, 30_000)

// An unconfirmed send may have stored the message, so `envoy-send` neither calls it failed nor
// exits with a refusal's 1: it exits 75, names the key, and prints the command that sends it again
// under that key, which the listener drops as a repeat if the first copy arrived. Before, it
// printed "failed", exited 1 and took no key, so the only resend it could make was a new message.
test("an unconfirmed envoy-send exits 75 and prints the command that resends it under the same key", async () => {
  requests.length = 0
  for (let n = 0; n < 2; n++) {
    answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
  }
  const first = await runEnvoySend(["ses_receiver", "it", "may", "have", "landed"])
  expect(first.code).toBe(75)
  expect(first.stderr).toStartWith("Claude Envoy send not confirmed: ")
  expect(first.stderr).not.toContain("failed")
  const key = String(requests[0]?.body["idempotency_key"])
  expect(key).toMatch(MINTED_KEY)
  const resend = mustResend(first.stderr)
  expect(resend).toBe(
    `cd ${scratch} && ENVOY_CONFIG_FILE=${first.file} bun ${resolve(import.meta.dir, "../bin/envoy-send.ts")} --idempotency-key ${key} ses_receiver 'it may have landed'`,
  )

  // The printed command, run as printed, sends the same message under the same key.
  const again = await runEnvoySend([], { command: resend })
  expect(again.code).toBe(0)
  expect(requests).toHaveLength(3)
  expect(requests.map((one) => one.body["idempotency_key"])).toEqual([key, key, key])
  expect(requests[2]?.body["message"]).toBe("it may have landed")
}, 30_000)

// F9 (the review of 215510405b): the printed resend carries what chose the listener. Run in a
// shell whose own settings name another listener (no ENVOY_CONFIG_FILE, an XDG_CONFIG_HOME whose
// envoy.json names a second one), it still goes to the listener the send went to.
test("the printed resend goes to the send's listener from a shell whose settings name another", async () => {
  requests.length = 0
  const other: Sent[] = []
  const elsewhere = Bun.serve({
    port: 0,
    fetch: async (request) => {
      other.push({
        path: new URL(request.url).pathname,
        authorization: request.headers.get("authorization"),
        body: JSON.parse(await request.text()) as Record<string, unknown>,
      })
      return Response.json(stored("ses_receiver"), { status: 202 })
    },
  })
  try {
    for (let n = 0; n < 2; n++) {
      answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
    }
    const first = await runEnvoySend(["ses_receiver", "to", "the", "first", "listener"])
    expect(first.code).toBe(75)
    const resend = mustResend(first.stderr)

    const xdg = join(scratch, "xdg-other")
    mkdirSync(join(xdg, "legion"), { recursive: true })
    writeFileSync(
      join(xdg, "legion", "envoy.json"),
      JSON.stringify({
        envoy: {
          enabled: true,
          url: `http://127.0.0.1:${elsewhere.port}`,
          token: fixtureSecret("other"),
        },
      }),
    )
    const again = await runEnvoySend([], {
      command: resend,
      env: { PATH: process.env["PATH"] ?? "", HOME: scratch, XDG_CONFIG_HOME: xdg },
    })
    expect(again.code).toBe(0)
    expect(other).toEqual([])
    expect(requests).toHaveLength(3)
    expect(new Set(requests.map((one) => one.body["idempotency_key"])).size).toBe(1)
  } finally {
    elsewhere.stop(true)
  }
}, 30_000)

// F8 (the review of 215510405b): a resend under a key the send before it named is a resend of a
// message that may have arrived, so its refusal says nothing of that earlier copy: not confirmed,
// exit 75, the key named and the resend printed again, never "failed".
test("a keyed resend the listener refuses is not confirmed, never failed", async () => {
  requests.length = 0
  answers.push(() => Response.json({ error: "no such session" }, { status: 404 }))
  const resend = await runEnvoySend(["--idempotency-key", "k-resend-1", "ses_receiver", "hello"])
  expect(resend.code).toBe(75)
  expect(resend.stderr).toStartWith(
    "Claude Envoy send not confirmed: this resend under idempotency_key k-resend-1 failed (",
  )
  expect(resend.stderr).toContain("and the send under that key before it may have arrived")
  expect(resend.stderr).not.toContain("Claude Envoy send failed")
  expect(printedResend(resend.stderr)).toContain("--idempotency-key k-resend-1 ses_receiver hello")
  expect(requests.map((one) => one.body["idempotency_key"])).toEqual(["k-resend-1"])
}, 30_000)

// F6 (the review of 215510405b): where the message holds a secret this process withholds, the
// printed line would read `<withheld>` in its place, and run as printed it would send a different
// message under the same key. So no ready command is printed: the key, and how to resend the
// original.
test("an unconfirmed send whose message holds a held secret prints the key and no ready command", async () => {
  requests.length = 0
  for (let n = 0; n < 2; n++) {
    answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
  }
  const token = fixtureSecret("listener")
  const first = await runEnvoySend(["ses_receiver", "the", "token", "is", token], { token })
  expect(first.code).toBe(75)
  const key = String(requests[0]?.body["idempotency_key"])
  expect(printedResend(first.stderr)).toBeUndefined()
  expect(first.stderr).toContain(
    `run envoy-send again from ${scratch} with --idempotency-key ${key}, the same target and the same message`,
  )
  expect(first.stderr).toContain("a printed copy could not reproduce the message")
  expect(first.stderr).not.toContain(token)
}, 30_000)

// h2 (the review of 2fa6fbf7cf): an ENVOY_URL whose user info is shorter than a held secret's 10
// characters passed the scrub, and the printed resend carried it whole. Now a URL setting with
// user info of any length, or any other URL no line may name, leaves the resend unprinted.
test("an unconfirmed send whose ENVOY_URL has short user info prints none of it", async () => {
  requests.length = 0
  for (let n = 0; n < 2; n++) {
    answers.push(() => Response.json({ error: "listener restarting" }, { status: 503 }))
  }
  const file = envoyConfig(fixtureSecret("listener"))
  const first = await runEnvoySend(["ses_receiver", "hello"], {
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: scratch,
      ENVOY_CONFIG_FILE: file,
      // Built from parts: the push's leak check refuses a literal URL with user info.
      ENVOY_URL: `http://${["ab", "x7q"].join(":")}@127.0.0.1:${server.port}`,
    },
  })
  expect(first.code).toBe(75)
  const key = String(requests[0]?.body["idempotency_key"])
  expect(printedResend(first.stderr)).toBeUndefined()
  expect(first.stderr).toContain(
    `run envoy-send again from ${scratch} with --idempotency-key ${key}, the same target and the same message, with the settings this run had: the command is not printed, since it holds ENVOY_URL (not printed: it carries a credential or is not a plain http(s) URL).\n`,
  )
  expect(first.stderr).not.toContain("x7q")
  expect(first.stderr).not.toContain("ab:")
}, 30_000)

test("a send whose only attempt was answered with an unreadable 2xx names that one attempt", async () => {
  requests.length = 0
  answers.push(() => new Response("stored, but not as JSON", { status: 202 }))
  const refused = await sendEnvoyMessage({
    baseUrl: `http://127.0.0.1:${server.port}`,
    credential: undefined,
    targetSession: "ses_receiver",
    message: "one attempt",
  }).catch((error: unknown) => error)
  expect(refused).toBeInstanceOf(EnvoySendUnconfirmedError)
  expect((refused as EnvoySendUnconfirmedError).attempts).toBe(1)
  expect((refused as Error).message).toContain("on its only attempt")
})

/** The sentence of the listener's 429 for a session past its sending limit
 *  (`senderRateLimitedAnswer`, packages/envoy/cmd/listener/sender_limit.go). */
const RATE_LIMITED_SENTENCE =
  "Your sending is rate limited. This message was not sent. This listener lets one session send " +
  "4 messages a minute and 2 at once. You can send it again after 15 seconds."

test("envoy-send whose sender is rate limited sends its session as source_session, prints the listener's sentence and exits 1", async () => {
  requests.length = 0
  answers.push(() =>
    Response.json(
      {
        error: RATE_LIMITED_SENTENCE,
        reason: "sender_rate_limited",
        retry_after_seconds: 15,
        messages_per_minute: 4,
        burst: 2,
      },
      { status: 429, headers: { "Retry-After": "15" } },
    ),
  )
  const token = fixtureSecret("listener")
  const sent = await runEnvoySend(["ses_receiver", "limited", "send"], {
    token,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: scratch,
      ENVOY_CONFIG_FILE: envoyConfig(token),
      ENVOY_SESSION_ID: "ses_sender",
    },
  })
  expect(sent.code).toBe(1)
  expect(sent.stderr).toBe(
    `Claude Envoy send failed: ${RATE_LIMITED_SENTENCE} (reason: sender_rate_limited)\n`,
  )
  // One request: a refusal is not sent again.
  expect(requests).toEqual([
    {
      path: "/v1/messages/send",
      authorization: `Bearer ${token}`,
      body: {
        source: "agent",
        source_session: "ses_sender",
        target_session: "ses_receiver",
        message: "limited send",
        idempotency_key: expect.stringMatching(MINTED_KEY),
      },
    },
  ])
})

test("envoy-send with no Envoy configured refuses, saying why, and sends nothing", async () => {
  requests.length = 0
  const missing = join(scratch, "absent", "envoy.json")
  const sent = await runEnvoySend(["ses_receiver", "nothing"], {
    env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: missing },
  })
  expect(sent.code).toBe(1)
  expect(sent.stderr).toBe(
    `Claude Envoy send failed: Envoy is off: no envoy.json at ${missing}: Envoy is off\n`,
  )
  expect(requests).toEqual([])
})

// The transport every client sends through follows no redirect (`EnvoyRedirectError`,
// `@legion/envoy-client/transport`): fetch would send the listener's bearer or the broker's DPoP
// proof, and the message, on to the origin a 3xx names. Through the command a person runs, a 3xx
// fails the send naming the status and where it pointed, exits 1 and prints no `Sent`, at each
// code, and is not sent again. Before, the send followed it and printed `Sent` on the other
// origin's answer.
test("envoy-send answered with a redirect exits 1 naming it, sends nothing to the other origin, and prints no Sent", async () => {
  let asked = 0
  const servers = redirectServers({
    redirects: () => {
      asked += 1
      return true
    },
    atElsewhere: () => new Response(null, { status: 202 }),
  })
  const file = join(scratch, "redirected-envoy.json")
  writeFileSync(
    file,
    JSON.stringify({
      envoy: { enabled: true, url: servers.origin, token: fixtureSecret("bearer") },
    }),
  )
  try {
    for (const code of REDIRECT_CODES) {
      servers.code = code
      asked = 0
      const {
        code: exitCode,
        stdout,
        stderr,
      } = await runEnvoySend(["ses_receiver", "redirected"], {
        env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: file },
      })
      const sent = { exitCode, stdout, stderr }
      expect(servers.received, `the other origin after a ${code}`).toEqual([])
      expect({ ...sent, asked }).toEqual({
        exitCode: 1,
        stdout: "",
        stderr: `Claude Envoy send failed: POST ${servers.origin}/v1/messages/send answered ${code}, redirecting to ${servers.elsewhere}, and the Envoy client follows no redirect: set the Envoy URL (ENVOY_URL, or envoy.json's envoy.url) to the listener's own address\n`,
        asked: 1,
      })
    }
  } finally {
    servers.stop()
  }
}, 30_000)

// A listener URL may carry a user and a password, which the send keeps; no line it prints does:
// the transport names each request on such a URL by the setting's fixed phrase alone
// (`requestForText`). The password is made for this run, so no line of the test holds a
// credential.
test("envoy-send prints no user and no password of its Envoy URL, whatever the listener answers", async () => {
  const password = crypto.randomUUID()
  const answering = (status: number) =>
    Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Response("no", { status }) })
  const servers = [401, 403, 404, 500].map((status) => ({ status, server: answering(status) }))
  // A listener that drops each connection unanswered, and a port nothing listens on.
  const listen = async (server: Server) => {
    const listening = Promise.withResolvers<void>()
    server.listen(0, "127.0.0.1", listening.resolve)
    await listening.promise
    const address = server.address()
    if (address === null || typeof address === "string") throw new Error("no port")
    return address.port
  }
  const dropping = createServer((socket) => socket.destroy())
  const dropped = await listen(dropping)
  const closed = createServer()
  const refused = await listen(closed)
  closed.close()
  try {
    const printed: Record<string, { exit: number; holdsUserinfo: boolean; line: unknown }> = {}
    const want: typeof printed = {}
    const request = `POST the configured Envoy URL ${URL_NOT_PRINTED}`
    for (const [answer, port, exit, wanted] of [
      ...servers.map(
        ({ status, server }) =>
          [
            String(status),
            server.port,
            // A 500 is tried again, and a send whose last answer is a 500 is not confirmed
            // delivered (exit 75): its line holds the refusal's own line.
            status === 500 ? 75 : 1,
            status === 500
              ? expect.stringContaining(
                  `(${request} failed with 500: a body that is not the listener's error JSON ("no"))`,
                )
              : `Claude Envoy send failed: ${request} failed with ${status}: a body that is not the listener's error JSON ("no")\n`,
          ] as const,
      ),
      // A connection dropped after the request went out may have been read, so the send is not
      // confirmed; one nothing took sent nothing. Each failure is named by its class.
      [
        "a dropped connection",
        dropped,
        75,
        expect.stringMatching(/^Claude Envoy send not confirmed: /),
      ] as const,
      [
        "a refused connection",
        refused,
        1,
        `Claude Envoy send failed: ${request} got no answer: ECONNREFUSED\n`,
      ] as const,
    ]) {
      const file = join(scratch, `userinfo-envoy-${answer}.json`)
      const url = `http://made-up-user:${password}@127.0.0.1:${port}`
      writeFileSync(
        file,
        JSON.stringify({ envoy: { enabled: true, url, token: fixtureSecret("bearer") } }),
      )
      const {
        code: exitCode,
        stdout,
        stderr,
      } = await runEnvoySend(["ses_receiver", "userinfo"], {
        env: { PATH: process.env["PATH"] ?? "", HOME: scratch, ENVOY_CONFIG_FILE: file },
      })
      const output = `${stdout}${stderr}`
      printed[answer] = {
        exit: exitCode,
        holdsUserinfo: output.includes(password) || output.includes("made-up-user"),
        line: output.replaceAll(password, "<the password>"),
      }
      want[answer] = { exit, holdsUserinfo: false, line: wanted }
    }
    expect(printed).toEqual(want)
  } finally {
    for (const { server } of servers) server.stop(true)
    dropping.close()
  }
}, 30_000)

// The Envoy URL may carry a user and a password. One the URL parser does not read as an http or
// https URL with a host turns Envoy off before any client is built, named by its length and the
// cause the one function found: the parser's own error would hold the value whole. Among them, a
// user part that holds a `/` or a `#` (a base64 token can) makes the parser read a host taken from
// it, here a listener of the test's own, which gets no request; and a value with a line ending at
// its end, as a file leaves it, is refused naming the line ending. The password is made for this
// run.
test("envoy-send whose Envoy URL is no http or https URL with a host, or holds an @ that no user part ends, sends nothing and names its length and cause", async () => {
  const password = crypto.randomUUID()
  let asked = 0
  const listener = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      asked += 1
      return Response.json({ ok: true })
    },
  })
  try {
    const values = [
      `made-up-user:${password}@127.0.0.1:1`,
      `mailto:made-up-user:${password}@127.0.0.1`,
      `//made-up-user:${password}@127.0.0.1:1`,
      `http://made-up-user:${password}/x@127.0.0.1:1`,
      `http://127.0.0.1:${listener.port}/${password}@envoy.example`,
      `http://127.0.0.1:${listener.port}#${password}@envoy.example`,
      `http://127.0.0.1:${listener.port}\n`,
    ]
    const said: { readonly exit: number; readonly line: string }[] = []
    const file = envoyConfig(fixtureSecret("listener"))
    for (const value of values) {
      const {
        code: exitCode,
        stdout,
        stderr,
      } = await runEnvoySend(["ses_receiver", "never sent"], {
        env: {
          PATH: process.env["PATH"] ?? "",
          HOME: scratch,
          ENVOY_CONFIG_FILE: file,
          ENVOY_URL: value,
        },
      })
      said.push({ exit: exitCode, line: `${stdout}${stderr}` })
    }
    expect({ said, asked }).toEqual({
      said: values.map((value) => ({
        exit: 1,
        line: `Claude Envoy send failed: Envoy is off: ENVOY_URL must be an http or https URL: ${unshownSettingUrl(value)}\n`,
      })),
      asked: 0,
    })
  } finally {
    listener.stop(true)
  }
}, 30_000)
