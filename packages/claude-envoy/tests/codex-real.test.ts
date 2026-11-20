// The Codex plugin on the real Codex CLI binary: a fresh `scripts/build.ts
// --codex-out` build of this tree, the `@sjawhar/codex-envoy` package, installed from a local
// marketplace into a Codex home of the test's own, run with `codex exec` against a stand-in for
// OpenAI's Responses API, whose turns scripts/e2e/lib/responses-stream.ts streams, as it streams
// the proofs' model stand-in's (scripts/e2e/lib/messages-standin.ts).
// It proves what no other test can: that Codex runs the plugin's SessionStart and PostToolUse hooks with the input this package
// reads, that the session record lands where the MCP server looks for it, that a message waiting
// in the inbox reaches the model in the very next request of the running turn, and that a side
// question runs in an ephemeral fork with the plugin off.
//
// `LEGION_TEST_CODEX` names the binary: the native `codex` of the release `/.codex-pin` pins, which
// CI installs (.github/actions/install-codex), or `~/.local/bin/codex` on a dev box. It must be the
// native binary and not the npm package's `bin/codex.js`, a Node process that runs the native one as
// its child: the session record lands under the hooks' parent, which would then not be the process
// this test starts. A run without one skips, except on GitHub Actions, where a skip would hide the
// only run of the check (GITHUB_ACTIONS, not CI: agent harnesses on the devbox export CI=true).
// Measured on Codex CLI 0.154.0.

import { afterAll, beforeAll, describe, expect, test } from "bun:test"
import { mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { join, resolve } from "node:path"
import { requireOrSkip } from "@legion/test-fixtures/host-tools"
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories"
import type { Server } from "bun"
import {
  type ResponsesItem,
  responsesCall,
  responsesMessage,
  responsesStream,
} from "../../../scripts/e2e/lib/responses-stream"
import { askSideQuestion } from "../src/codex/codex-cli"
import { codexInboxDirectory, putInbox, takenBy } from "../src/codex/codex-inbox"
import { codexSessionRecordFile, readCodexSessionRecord } from "../src/codex/codex-session-record"

const codex = process.env["LEGION_TEST_CODEX"]
const skipped = requireOrSkip(
  "LEGION_TEST_CODEX, naming the native Codex binary",
  codex !== undefined,
  "claude-envoy",
)
const packageRoot = resolve(import.meta.dir, "..")
const root = temporaryDirectory("codex-real")
const WAIT_MS = 60_000

interface ModelRequest {
  readonly input: readonly Record<string, unknown>[]
}

describe.skipIf(skipped)("the Codex plugin on the real Codex binary", () => {
  const requests: ModelRequest[] = []
  let model: Server<undefined> | undefined
  let codexHome = ""
  let work = ""
  let pluginData = ""
  /** What a test does before the stand-in answers a request of a turn, given how many of the
   *  turn's tool calls have their output in it. */
  let beforeAnswer: (outputs: number) => Promise<void> = async () => {}
  /** Answers each request: a tool call for a prompt holding `RUN: <command>`, a second one for its
   *  `THEN: <command>` once the first call's output is back (`outputs`), else an echo. */
  const answer = (input: readonly Record<string, unknown>[], outputs: number): ResponsesItem[] => {
    const user = [...input].reverse().find((item) => item["role"] === "user")
    const text = JSON.stringify(user?.["content"] ?? "")
    const command = /RUN: ([^"\\]*?)(?: THEN: ([^"\\]*))?"/.exec(text)?.[outputs + 1]
    if (command !== undefined) {
      return [
        responsesCall(
          `fc_${requests.length}`,
          `call_${requests.length}`,
          "exec_command",
          JSON.stringify({ cmd: command }),
        ),
      ]
    }
    return [responsesMessage(`msg_${requests.length}`, `ECHO ${requests.length}`)]
  }

  beforeAll(async () => {
    model = Bun.serve({
      async fetch(request) {
        const body = (await request.json()) as ModelRequest
        requests.push(body)
        const input = body.input ?? []
        const outputs = input.filter((item) => item["type"] === "function_call_output").length
        await beforeAnswer(outputs)
        return new Response(responsesStream(`resp_${requests.length}`, answer(input, outputs)), {
          headers: { "content-type": "text/event-stream" },
        })
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
        // Off, so the stand-in's command runs on any host: Codex's Linux sandbox re-executes Codex
        // inside bubblewrap as `codex-linux-sandbox`, which fails on a host whose bubblewrap cannot
        // find that name (measured on a dev box under 0.154.0), and the test would then see a
        // failed call. The command is the test's own and the plugin's hooks are what it proves.
        'sandbox_mode = "danger-full-access"',
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
            name: "codex-envoy",
            policy: { authentication: "ON_INSTALL", installation: "AVAILABLE" },
            source: { path: "./plugins/codex-envoy", source: "local" },
          },
        ],
      }),
    )
    const build = Bun.spawnSync(
      [process.execPath, "scripts/build.ts", "--codex-out", join(market, "plugins", "codex-envoy")],
      { cwd: packageRoot, stderr: "pipe" },
    )
    if (build.exitCode !== 0) throw new Error(`build failed: ${build.stderr.toString()}`)
    for (const argv of [
      ["plugin", "marketplace", "add", market],
      ["plugin", "add", "codex-envoy@legion-test"],
    ]) {
      const ran = Bun.spawnSync([codex as string, ...argv], { env: environment(), stderr: "pipe" })
      if (ran.exitCode !== 0) throw new Error(`codex ${argv.join(" ")}: ${ran.stderr.toString()}`)
    }
    pluginData = join(codexHome, "plugins", "data", "codex-envoy-legion-test")
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
          "RUN: sleep 4; echo slept THEN: echo again",
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
      // The line goes into the inbox once the first call's output is back, before the answer that
      // makes the second call, so it waits there while a call of the running turn runs. Not as
      // soon as the record is there: the plugin's MCP server, once it reads the record (it polls
      // it every 250 ms, src/codex/codex-server.ts), discards every line already waiting, as lines
      // an earlier owner of the session left, and the first call's four seconds are past that read.
      let name: string | undefined
      beforeAnswer = async (outputs) => {
        if (outputs === 1 && name === undefined)
          name = await putInbox(inbox, { content: "INBOX LINE FOR THE RUNNING TURN" })
      }
      expect(await exec.exited).toBe(0)
      beforeAnswer = async () => {}
      expect(name).toBeDefined()
      if (name === undefined) return
      expect(await takenBy(inbox, name)).toBe("hook")
      const turn = requests.slice(before)
      const carried = turn.findIndex((request) =>
        JSON.stringify(request.input).includes("INBOX LINE FOR THE RUNNING TURN"),
      )
      expect(carried).toBeGreaterThan(0)
      // The request after the second tool call, not a later turn: it carries both calls' outputs,
      // the second's last, which is that command's own output and not the prompt's text.
      const outputs = (turn[carried]?.input ?? []).filter(
        (item) => item["type"] === "function_call_output",
      )
      expect(outputs).toHaveLength(2)
      expect(JSON.stringify(outputs.at(-1))).toMatch(/exited with code 0[\s\S]*again/)
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
