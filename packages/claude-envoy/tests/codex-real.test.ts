// The Codex plugin on the real Codex CLI binary (INFRA-3558): a fresh `scripts/build.ts --out`
// build of this tree installed from a local marketplace into a Codex home of the test's own, run
// with `codex exec` against a stand-in for OpenAI's Responses API. It proves what no other test
// can: that Codex runs the plugin's SessionStart and PostToolUse hooks with the input this package
// reads, that the session record lands where the MCP server looks for it, that a message waiting
// in the inbox reaches the model in the very next request of the running turn, and that a side
// question runs in an ephemeral fork with the plugin off.
//
// `LEGION_TEST_CODEX` names the binary (`~/.local/bin/codex` on a dev box); without it the file
// skips, and nothing in CI installs Codex. Measured on Codex CLI 0.154.0.

import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import type { Server } from "bun"
import { askSideQuestion } from "../src/codex/codex-cli"
import { codexInboxDirectory, putInbox, takenBy } from "../src/codex/codex-inbox"
import { codexSessionRecordFile, readCodexSessionRecord } from "../src/codex/codex-session-record"

const codex = process.env["LEGION_TEST_CODEX"]
const packageRoot = resolve(import.meta.dir, "..")
const root = temporaryDirectory("codex-real")
const WAIT_MS = 60_000

interface ModelRequest {
  readonly input: readonly Record<string, unknown>[]
}

describe.skipIf(codex === undefined)("the Codex plugin on the real Codex binary", () => {
  const requests: ModelRequest[] = []
  let model: Server<undefined> | undefined
  let codexHome = ""
  let work = ""
  let pluginData = ""
  /** Answers each request: a tool call for a prompt holding `RUN: <command>`, else an echo. */
  const answer = (input: readonly Record<string, unknown>[]): Record<string, unknown>[] => {
    const last = input.at(-1)
    const user = [...input].reverse().find((item) => item["role"] === "user")
    const text = JSON.stringify(user?.["content"] ?? "")
    const run = /RUN: ([^"\\]*)/.exec(text)
    if (run !== null && last?.["type"] !== "function_call_output") {
      return [
        {
          arguments: JSON.stringify({ cmd: run[1] }),
          call_id: `call_${requests.length}`,
          id: `fc_${requests.length}`,
          name: "exec_command",
          type: "function_call",
        },
      ]
    }
    return [
      {
        content: [{ annotations: [], text: `ECHO ${requests.length}`, type: "output_text" }],
        id: `msg_${requests.length}`,
        role: "assistant",
        status: "completed",
        type: "message",
      },
    ]
  }

  beforeAll(async () => {
    model = Bun.serve({
      async fetch(request) {
        const body = (await request.json()) as ModelRequest
        requests.push(body)
        const id = `resp_${requests.length}`
        const events: Record<string, unknown>[] = [{ response: { id }, type: "response.created" }]
        for (const item of answer(body.input ?? [])) events.push({ item, type: "response.output_item.done" })
        events.push({
          response: {
            id,
            usage: {
              input_tokens: 1,
              input_tokens_details: { cached_tokens: 0 },
              output_tokens: 1,
              output_tokens_details: { reasoning_tokens: 0 },
              total_tokens: 2,
            },
          },
          type: "response.completed",
        })
        return new Response(
          events.map((event) => `event: ${event["type"]}\ndata: ${JSON.stringify(event)}\n\n`).join(""),
          { headers: { "content-type": "text/event-stream" } },
        )
      },
      port: 0,
    })
    codexHome = join(root, "codex-home")
    work = join(root, "work")
    const market = join(root, "market")
    mkdirSync(codexHome, { recursive: true })
    mkdirSync(work, { recursive: true })
    mkdirSync(join(market, ".agents", "plugins"), { recursive: true })
    writeFileSync(
      join(codexHome, "config.toml"),
      [
        'model = "standin-model"',
        'model_provider = "standin"',
        'approval_policy = "never"',
        'sandbox_mode = "workspace-write"',
        "[model_providers.standin]",
        'name = "standin"',
        `base_url = "http://127.0.0.1:${model.port}/v1"`,
        'wire_api = "responses"',
        "requires_openai_auth = false",
        `[projects."${work}"]`,
        'trust_level = "trusted"',
      ].join("\n"),
    )
    writeFileSync(
      join(market, ".agents", "plugins", "marketplace.json"),
      JSON.stringify({
        name: "legion-test",
        plugins: [
          {
            category: "Productivity",
            name: "claude-envoy",
            policy: { installation: "AVAILABLE" },
            source: { path: "./plugins/claude-envoy", source: "local" },
          },
        ],
      }),
    )
    const build = Bun.spawnSync(
      [process.execPath, "scripts/build.ts", "--out", join(market, "plugins", "claude-envoy")],
      { cwd: packageRoot, stderr: "pipe" },
    )
    if (build.exitCode !== 0) throw new Error(`build failed: ${build.stderr.toString()}`)
    for (const argv of [
      ["plugin", "marketplace", "add", market],
      ["plugin", "add", "claude-envoy@legion-test"],
    ]) {
      const ran = Bun.spawnSync([codex as string, ...argv], { env: environment(), stderr: "pipe" })
      if (ran.exitCode !== 0) throw new Error(`codex ${argv.join(" ")}: ${ran.stderr.toString()}`)
    }
    pluginData = join(codexHome, "plugins", "data", "claude-envoy-legion-test")
  }, 300_000)

  afterAll(() => {
    model?.stop(true)
  })

  /** Codex's environment: this test's Codex home and a home with no envoy.json, so the plugin's
   *  MCP server finds Envoy off and touches no listener. */
  function environment(): Record<string, string> {
    return {
      CODEX_HOME: codexHome,
      HOME: join(root, "home"),
      PATH: process.env["PATH"] ?? "",
      XDG_CONFIG_HOME: join(root, "home", ".config"),
      XDG_STATE_HOME: join(root, "home", ".local", "state"),
    }
  }

  test(
    "SessionStart records the session; a waiting message reaches the running turn at its next tool call",
    async () => {
      const before = requests.length
      const exec = Bun.spawn(
        [
          codex as string,
          "exec",
          "--skip-git-repo-check",
          "--dangerously-bypass-hook-trust",
          "RUN: sleep 4; echo slept",
        ],
        { cwd: work, env: environment(), stderr: "pipe", stdout: "pipe" },
      )
      // The record lands under the codex process, the hook's parent.
      const recordFile = codexSessionRecordFile(pluginData, exec.pid)
      const deadline = Date.now() + WAIT_MS
      let record = await readCodexSessionRecord(recordFile)
      while (record === undefined && Date.now() < deadline) {
        await Bun.sleep(100)
        record = await readCodexSessionRecord(recordFile)
      }
      expect(record).toMatchObject({ codex_home: codexHome, cwd: work, source: "startup" })
      if (record === undefined) return
      expect(record.transcript_path).toContain(record.session_id)
      const inbox = codexInboxDirectory(pluginData, record.session_id)
      const name = await putInbox(inbox, { content: "INBOX LINE FOR THE RUNNING TURN" })
      expect(await exec.exited).toBe(0)
      expect(await takenBy(inbox, name)).toBe("hook")
      const turn = requests.slice(before)
      const carried = turn.findIndex((request) =>
        JSON.stringify(request.input).includes("INBOX LINE FOR THE RUNNING TURN"),
      )
      // The request after the tool call, not a later turn: it also carries the call's output.
      expect(carried).toBeGreaterThan(0)
      expect(JSON.stringify(turn[carried]?.input)).toContain("slept")
      // The dispatch-first skill and the session note reach the model only with Dispatch on; this
      // home has none, so the record is all the session-start hook was asked for.
      expect(readFileSync(record.transcript_path ?? "", "utf8")).toContain("task_complete")
    },
    WAIT_MS * 2,
  )

  test(
    "a side question runs in an ephemeral fork with the plugin off and leaves the session's transcript alone",
    async () => {
      const files = [...new Bun.Glob("**/*.jsonl").scanSync(join(codexHome, "sessions"))]
      const recordFiles = [...new Bun.Glob("codex-sessions/*.json").scanSync(pluginData)]
      const record = await readCodexSessionRecord(join(pluginData, recordFiles[0] ?? ""))
      expect(record).toBeDefined()
      if (record === undefined) return
      const transcript = readFileSync(record.transcript_path ?? "", "utf8")
      const answerText = await askSideQuestion(
        { codexHome, env: environment(), executable: codex as string },
        record.session_id,
        "BTW what is this?",
        WAIT_MS,
      )
      expect(answerText).toMatch(/^ECHO \d+$/)
      expect(readFileSync(record.transcript_path ?? "", "utf8")).toBe(transcript)
      expect([...new Bun.Glob("**/*.jsonl").scanSync(join(codexHome, "sessions"))]).toEqual(files)
      expect([...new Bun.Glob("codex-sessions/*.json").scanSync(pluginData)]).toEqual(recordFiles)
    },
    WAIT_MS * 2,
  )
})
