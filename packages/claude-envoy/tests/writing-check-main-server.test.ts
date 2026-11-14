/**
 * The plugin's own writes against a Dispatch from before the writing check (INFRA-3231, R14;
 * check 4370789f), in a Claude Code session: the router of a person's messages
 * (`../src/user-turn-router`) and the channel server's reply (`postDispatchReply`,
 * `../src/envoy-channel-server`). Main's server decodes every JSON body with
 * `DisallowUnknownFields` and answers a member it does not declare `400 INVALID_JSON`, and a
 * plugin release can reach a host before the server that knows `override_writing_check` does. So
 * a plugin sends the field only on a second send, after the check refused the first, and against
 * main every first send is written.
 *
 * The server here stands in for main's rather than being main's own: building main's server would
 * need a second checkout of main and a Go build of it in every run of this file, while what these
 * rows read of main is its strict decode alone, which the stand-in keeps member for member on the
 * reply route the plugin writes to (main's `messageReplyInput`). Its accept route answers as
 * Dispatch's does. The writing-check suite beside this one (`writing-check.live.test.ts`) runs the
 * same paths against a server of this checkout with the check on.
 */
import { afterAll, beforeAll, beforeEach, describe, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { bearerCredential } from "@legion/envoy-client/bearer"
import { type DispatchDelivery, renderInbound } from "@legion/envoy-client/delivery"
import { DispatchClient } from "@legion/envoy-client/dispatch-http"
import { sessionConfigFile } from "@legion/envoy-client/session-settings"
import type { Server } from "bun"
import type { ModCallArguments, ModCallName, ModCallResults } from "../hooks/mod-protocol"
import { postDispatchReply } from "../src/envoy-channel-server"
import { modSession } from "../src/mod-session"
import { SessionIdentity } from "../src/session-identity"
import { createUserTurnRouter } from "../src/user-turn-router"
import { readUserTurnState, userTurnState, userTurnStateFile } from "../src/user-turn-state"
import { waitFor } from "./channel-session-harness"
import { isolatePaneEnvironment } from "./pane-environment"

isolatePaneEnvironment()

const SESSION = "ses_claude_main"
const DIRECT = `notifications.agent.${SESSION}`
const PERSON = "alice"
const TOKEN = "main-dispatch-token"
/** The members main's server declares for a reply's body; it refuses any other. */
const MAIN_REPLY_MEMBERS: readonly string[] = [
  "actor",
  "attempt",
  "body",
  "error",
  "in_reply_to_block",
]

/** A person's direct message `id` to this session, as the listener publishes it. */
function personSends(id: string, body: string, mode: "steer" | "btw"): string {
  return JSON.stringify({
    event_id: `evt-${id}`,
    source: "dispatch",
    source_event_id: `src-${id}`,
    topic: DIRECT,
    dedupe_key: `agent.${SESSION}.${id}:${mode}`,
    issued_at: 1_760_000_000_000,
    payload_summary: body,
    payload: JSON.stringify({
      event: {
        actor: { id: PERSON, kind: "user" },
        issue_key: null,
        payload: {
          author: { id: PERSON, kind: "user" },
          body,
          created_at: "2026-09-30T00:00:00Z",
          deliveries: [],
          id,
          in_reply_to: null,
          issue_key: null,
          target: `session:${SESSION}`,
        },
        type: "message.created",
      },
      delivery: { attempt: 1, mode },
    }),
    trace_id: `trace-${id}`,
  })
}

describe("the plugin's own writes against a Dispatch that has no writing check", () => {
  /** Each reply the stand-in decoded, with the status it answered. */
  const written: { readonly path: string; readonly status: number; readonly body: unknown }[] = []
  /** What each direct message's accept hands back: the line the person sent. */
  const lines = new Map<string, string>()
  let standIn: Server<undefined> | undefined
  const url = (): string => {
    if (standIn === undefined) throw new Error("the stand-in Dispatch did not start")
    return `http://127.0.0.1:${standIn.port}`
  }

  beforeAll(() => {
    standIn = Bun.serve({
      port: 0,
      fetch: async (request) => {
        const path = new URL(request.url).pathname
        const parts = path.split("/")
        if (request.method === "POST" && path.endsWith("/accept")) {
          const id = parts.at(-4) ?? ""
          return Response.json({
            message_id: id,
            attempt: Number(parts.at(-2)),
            body: lines.get(id) ?? "",
            delivery: "steer",
            accepted_as: "user_turn",
          })
        }
        if (request.method === "POST" && /^\/api\/v1\/messages\/[^/]+\/reply$/.test(path)) {
          const body = (await request.json()) as Record<string, unknown>
          const unknown = Object.keys(body).find((member) => !MAIN_REPLY_MEMBERS.includes(member))
          if (unknown !== undefined) {
            written.push({ path, status: 400, body })
            return Response.json(
              {
                code: "INVALID_JSON",
                error: `invalid JSON body: json: unknown field "${unknown}"`,
              },
              { status: 400 },
            )
          }
          written.push({ path, status: 201, body })
          return Response.json({}, { status: 201 })
        }
        return Response.json({ code: "NOT_FOUND", error: "not found" }, { status: 404 })
      },
    })
  })
  afterAll(() => {
    standIn?.stop(true)
  })
  beforeEach(() => {
    written.length = 0
    // The session's Dispatch, as `postDispatchReply` reads it: this stand-in, under a token.
    process.env["DISPATCH_URL"] = url()
    process.env["DISPATCH_TOKEN"] = TOKEN
  })

  /** A person's direct message to this session, whose accept hands back `line`. */
  const directMessage = (line: string): string => {
    const id = crypto.randomUUID()
    lines.set(id, line)
    return id
  }
  /** Every write the stand-in decoded for message `id`'s reply, once the first has arrived. */
  const repliesTo = async (id: string) => {
    const path = `/api/v1/messages/${id}/reply`
    await waitFor(async () => written.some((write) => write.path === path), `the reply to ${id}`)
    return written.filter((write) => write.path === path)
  }
  const actor = { kind: "session", id: SESSION }

  /** A router whose Claude Code answers as `answers` says, replying as the channel server does. */
  const router = async (answers: {
    readonly fork?: (prompt: string) => ModCallResults["model.fork"]
    readonly command?: () => ModCallResults["command.run"]
  }) => {
    const directory = await mkdtemp(join(tmpdir(), "claude-envoy-main-server-"))
    const stateFile = userTurnStateFile(directory, SESSION)
    const identity = new SessionIdentity(SESSION, directory)
    const configFile = sessionConfigFile("claude", process.env)
    const link = {
      connected: () => true,
      request: async <N extends ModCallName>(
        call: N,
        args: ModCallArguments[N],
      ): Promise<ModCallResults[N]> => {
        if (call === "model.fork" && answers.fork !== undefined) {
          const { prompt } = args as ModCallArguments["model.fork"]
          return answers.fork(prompt) as ModCallResults[N]
        }
        if (call === "command.run" && answers.command !== undefined) {
          return answers.command() as ModCallResults[N]
        }
        throw new Error(`the mod was asked ${call}`)
      },
    }
    const routed = createUserTurnRouter({
      dispatch: (ms) =>
        new DispatchClient(url(), bearerCredential(TOKEN), fetch, AbortSignal.timeout(ms)),
      effortPicked: () => undefined,
      refreshSettings: () => undefined,
      reply: (delivery: DispatchDelivery, result) =>
        postDispatchReply(identity, configFile, delivery, result),
      toMainTurn: () => undefined,
      session: modSession({ link, warn: () => undefined, commandOutputMs: 10, commandEchoMs: 10 }),
      sessionId: () => SESSION,
      state: userTurnState(stateFile, await readUserTurnState(stateFile)),
      warn: () => undefined,
      commandWaitingMs: 60_000,
    })
    return {
      directory,
      identity,
      configFile,
      route: (id: string, body: string, mode: "steer" | "btw") =>
        routed.route(renderInbound(personSends(id, body, mode), SESSION, DIRECT)),
    }
  }

  test("a slash command's output is written on its first send, with no field", async () => {
    const id = directMessage("/status")
    const session = await router({ command: () => ({ text: "Context: 12% used." }) })
    try {
      expect(await session.route(id, "/status", "steer")).toBe(true)
      expect(await repliesTo(id)).toEqual([
        {
          path: `/api/v1/messages/${id}/reply`,
          status: 201,
          body: { actor, attempt: 1, body: "Context: 12% used." },
        },
      ])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  })

  test("a fork's failure, the plugin's own error reply, is written on its first send, with no field", async () => {
    const id = directMessage("Why was the first read slow?")
    const session = await router({
      fork: () => ({ isAnswered: false as const, reason: "the fork failed" }),
    })
    try {
      await session.route(id, "Why was the first read slow?", "btw")
      expect(await repliesTo(id)).toEqual([
        {
          path: `/api/v1/messages/${id}/reply`,
          status: 201,
          body: { actor, attempt: 1, error: "the fork failed" },
        },
      ])
    } finally {
      await rm(session.directory, { recursive: true, force: true })
    }
  })

  // The channel server's own error for a frame it could not read, as it posts it.
  test("the channel server's own error reply is written on its first send, with no field", async () => {
    const id = directMessage("Why was the first read slow?")
    const delivery: DispatchDelivery = {
      resource: "message",
      id,
      attempt: 1,
      mode: "btw",
      replyPath: `/api/v1/messages/${id}/reply`,
      replyFields: {},
      issueKey: null,
      body: "Why was the first read slow?",
    }
    expect(
      await postDispatchReply(
        new SessionIdentity(SESSION, process.cwd()),
        sessionConfigFile("claude", process.env),
        delivery,
        { error: "Invalid Dispatch targeted delivery frame", generated: true },
      ),
    ).toEqual({ posted: true })
    expect(await repliesTo(id)).toEqual([
      {
        path: `/api/v1/messages/${id}/reply`,
        status: 201,
        body: { actor, attempt: 1, error: "Invalid Dispatch targeted delivery frame" },
      },
    ])
  })

  // The negative control: the stand-in decodes as main does, so the same reply carrying the
  // field is refused, which is what a first send carrying it would get from main's server.
  test("a reply carrying override_writing_check is refused 400 INVALID_JSON", async () => {
    const id = directMessage("Why was the first read slow?")
    const refused = await new DispatchClient(url(), bearerCredential(TOKEN))
      .deliveryReply("message", id, {
        actor,
        attempt: 1,
        error: "the fork failed",
        override_writing_check: "generated",
      })
      .catch((error: unknown) => error)
    expect(refused).toMatchObject({ code: "INVALID_JSON", status: 400 })
    expect(written.map(({ status }) => status)).toEqual([400])
  })
})
