import { afterAll, beforeEach, expect, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"

// The written rules under Claude Code (INFRA-3231, R6): the `dispatch` command refuses once the
// session's first write of a comment, and its first after each compaction, showing the rules
// Dispatch sends for that write's reader. Claude Code reports a compaction as a SessionStart whose
// source is `compact`, and the plugin's open-asks hook counts it in the command's state file. The
// hook and the command run here as Claude Code runs them, each a process of its own, against a
// stand-in Dispatch.

const RULES_TOKEN = fixtureSecret("dsp")
const hookEntry = resolve(import.meta.dir, "..", "hooks", "session-hook.ts")
const dispatchEntry = resolve(import.meta.dir, "..", "..", "envoy-client", "bin", "dispatch.ts")
const SESSION = "s-rules"
const RULES =
  "# Write so a reader understands it on the first read\n\nThe rules the stand-in Dispatch sends.\n"

/** What the stand-in Dispatch says about the refusal, and every request it answered. */
const dispatch = {
  refusal: "on" as "on" | "off",
  requests: [] as { readonly method: string; readonly path: string; readonly body: unknown }[],
}

const server = Bun.serve({
  port: 0,
  fetch: async (request) => {
    const { pathname } = new URL(request.url)
    const text = await request.text()
    const body: unknown = text === "" ? undefined : JSON.parse(text)
    dispatch.requests.push({ method: request.method, path: pathname, body })
    if (pathname === "/api/v1/settings/writing-rules" && request.method === "GET") {
      return Response.json({ refusal: dispatch.refusal, extra: [] })
    }
    if (pathname === "/api/v1/writing-rules" && request.method === "POST") {
      return Response.json({ reader: "ann@example.com", text: RULES })
    }
    if (pathname === "/api/v1/issues/DSP-1/comments" && request.method === "POST") {
      return Response.json({
        id: `comment-${dispatch.requests.length}`,
        issue_key: "DSP-1",
        advice: { issue_status: "in_progress", session_writes_since_human: 0, your_open_asks: [] },
      })
    }
    if (pathname === "/api/v1/asks/open") {
      return Response.json({
        session_id: SESSION,
        as_of: "2026-11-15T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      })
    }
    return Response.json(
      { code: "NOT_FOUND", error: `no route for ${request.method} ${pathname}` },
      { status: 404 },
    )
  },
})
afterAll(() => server.stop(true))

let scratch = ""
beforeEach(async () => {
  if (scratch !== "") await rm(scratch, { recursive: true, force: true })
  scratch = await mkdtemp(join(tmpdir(), "claude-envoy-writing-rules-"))
  dispatch.refusal = "on"
  dispatch.requests.length = 0
})
afterAll(async () => {
  if (scratch !== "") await rm(scratch, { recursive: true, force: true })
})

/** The environment Claude Code gives a Bash command and a hook of the session. */
function environment(extra: Record<string, string> = {}): Record<string, string> {
  return {
    PATH: process.env["PATH"] ?? "",
    HOME: scratch,
    CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    DISPATCH_STATE_DIR: join(scratch, "state"),
    DISPATCH_URL: `http://127.0.0.1:${server.port}`,
    DISPATCH_TOKEN: RULES_TOKEN,
    ...extra,
  }
}

async function finished(
  child: Bun.Subprocess<"pipe" | "ignore" | Uint8Array, "pipe", "pipe">,
): Promise<{ readonly code: number; readonly stdout: string; readonly stderr: string }> {
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ])
  return { code, stdout, stderr }
}

/** One `dispatch comment` in the session's Bash, as the agent sends it. */
function comment(body: string, extra: Record<string, string> = {}) {
  return finished(
    Bun.spawn([process.execPath, dispatchEntry, "comment", "--issue", "DSP-1", "--body", body], {
      cwd: scratch,
      env: environment({ DISPATCH_HOST: "claude", CLAUDE_CODE_SESSION_ID: SESSION, ...extra }),
      stdin: "ignore",
      stdout: "pipe",
      stderr: "pipe",
    }),
  )
}

/** The open-asks SessionStart hook for `source`, as Claude Code runs it. */
function sessionStart(source: "startup" | "compact") {
  return finished(
    Bun.spawn([process.execPath, hookEntry, "open-asks"], {
      env: environment(),
      stdin: new TextEncoder().encode(
        JSON.stringify({
          session_id: SESSION,
          source,
          hook_event_name: "SessionStart",
          cwd: scratch,
        }),
      ),
      stdout: "pipe",
      stderr: "pipe",
    }),
  )
}

const posted = () =>
  dispatch.requests.filter(
    (request) => request.method === "POST" && request.path === "/api/v1/issues/DSP-1/comments",
  )
const asked = (path: string) => dispatch.requests.filter((request) => request.path === path)

test("the first write is refused once with the rules, the second posts, and a compaction brings the refusal back once", async () => {
  expect((await sessionStart("startup")).code).toBe(0)

  const first = await comment("The deploy finished at 14:02.")
  expect(first.code).toBe(1)
  expect(first.stdout).toStartWith(
    "dispatch comment was not called: this call is the first write of this session",
  )
  expect(first.stdout).toContain(RULES.trimEnd())
  expect(posted()).toEqual([])
  expect(asked("/api/v1/writing-rules").map((request) => request.body)).toEqual([
    {
      route: "POST /api/v1/issues/DSP-1/comments",
      body: expect.objectContaining({ body: "The deploy finished at 14:02." }),
    },
  ])

  const second = await comment("The deploy finished at 14:02.")
  expect(second.code).toBe(0)
  expect(second.stdout).toContain("Posted comment")
  expect(posted()).toHaveLength(1)

  const compacted = await sessionStart("compact")
  expect(compacted.code).toBe(0)
  expect(compacted.stderr).not.toContain("not counted")

  const third = await comment("The rollback took 3 minutes.")
  expect(third.code).toBe(1)
  expect(third.stdout).toStartWith(
    "dispatch comment was not called: this call is the first write of this session",
  )
  expect(third.stdout).toContain(RULES.trimEnd())
  expect(posted()).toHaveLength(1)
  expect(asked("/api/v1/writing-rules").map((request) => request.body)).toEqual([
    {
      route: "POST /api/v1/issues/DSP-1/comments",
      body: expect.objectContaining({ body: "The deploy finished at 14:02." }),
    },
    {
      route: "POST /api/v1/issues/DSP-1/comments",
      body: expect.objectContaining({ body: "The rollback took 3 minutes." }),
    },
  ])

  const fourth = await comment("The rollback took 3 minutes.")
  expect(fourth.code).toBe(0)
  expect(posted()).toHaveLength(2)
  // The command kept Dispatch's answer about the refusal: one read served all four calls.
  expect(asked("/api/v1/settings/writing-rules")).toHaveLength(1)
}, 60_000)

test("with the refusal off, nothing is refused, before or after a compaction", async () => {
  dispatch.refusal = "off"
  // The steps of the row above: write, write, compaction, write, write. Each write posts.
  expect((await sessionStart("startup")).code).toBe(0)

  const first = await comment("The deploy finished at 14:02.")
  expect(first.code).toBe(0)
  expect(first.stdout).toContain("Posted comment")
  expect(posted()).toHaveLength(1)

  const second = await comment("The deploy finished at 14:02.")
  expect(second.code).toBe(0)
  expect(second.stdout).toContain("Posted comment")
  expect(posted()).toHaveLength(2)

  const compacted = await sessionStart("compact")
  expect(compacted.code).toBe(0)
  expect(compacted.stderr).not.toContain("not counted")

  const third = await comment("The rollback took 3 minutes.")
  expect(third.code).toBe(0)
  expect(third.stdout).toContain("Posted comment")
  expect(posted()).toHaveLength(3)

  const fourth = await comment("The rollback took 3 minutes.")
  expect(fourth.code).toBe(0)
  expect(fourth.stdout).toContain("Posted comment")
  expect(posted()).toHaveLength(4)
  expect(asked("/api/v1/writing-rules")).toEqual([])
}, 60_000)

test("a call a person typed (DISPATCH_PERSON_TYPED=1) is never refused, and asks nothing about the rules", async () => {
  const typed = await comment("The deploy finished at 14:02.", { DISPATCH_PERSON_TYPED: "1" })
  expect(typed.code).toBe(0)
  expect(posted()).toHaveLength(1)
  expect(asked("/api/v1/settings/writing-rules")).toEqual([])
  expect(asked("/api/v1/writing-rules")).toEqual([])
}, 60_000)
