import { beforeEach, expect, setDefaultTimeout, test } from "bun:test"
import { generateKeyPairSync } from "node:crypto"
import { existsSync, realpathSync } from "node:fs"
import { chmod, mkdir, mkdtemp, readdir, readFile, realpath, rm, writeFile } from "node:fs/promises"
import { tmpdir } from "node:os"
import { dirname, join, relative, resolve } from "node:path"
import { signedIn, writeSignIn } from "@legion/cipher-client/test/sign-ins"
import { RETRY_FIRST_WAIT_MS, TRADE_ATTEMPT_MS } from "@legion/cipher-client/token-client"
import { COMMAND_TIMEOUT_MS } from "@legion/envoy-client/envoy-config"
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret"
import { version as codexPackageVersion } from "../codex/package.json" with { type: "json" }
import { JJ_BASE_CHAIN_TIMEOUT_MS, OPEN_ASKS_TIMEOUT_MS } from "../hooks/open-asks-bounds"
import { version as packageVersion } from "../package.json" with { type: "json" }
import type { Census, TimedWait } from "./hook-timed-waits"
import { reachedModules, type WaitSite, waitSites } from "./hook-wait-sites"
import { collected, exitBeforeTheGuard, ROW_GUARD_MS, rowStarted } from "./row-deadline"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"
import { unresolvedNames } from "./unresolved-names"

const hookEntry = resolve(import.meta.dir, "..", "hooks", "session-hook.ts")
/** The Dispatch token the hook is handed, in its environment or in envoy.json. */
const DISPATCH_TOKEN = fixtureSecret("dsp")

// A row waits for its hooks to exit and decides on what they printed and wrote. A hook that never
// exits fails its row by name before the hang guard this file sets as its rows' default (`ROW_GUARD_MS`, 120 s, `./row-deadline.ts`). Fixed limits of 20 s and 30 s decided rows before: each hook first wrote
// Bun's transpiler cache into its empty HOME (`./transpiler-cache.ts`), and at loads of 790 to 960
// a hook did not exit within them.
beforeEach(rowStarted)
setDefaultTimeout(ROW_GUARD_MS)

/** Runs the hook as Claude Code does, with `env` as its whole environment (and Bun's transpiler
 * cache off, `./transpiler-cache.ts`) and `options.preload` given to Bun first where set. The
 * session's cwd is the test's scratch HOME unless the input names one: the hook reads
 * `<cwd>/.envoy/envoy.json`, so a shared directory's file would become every test's checkout. */
async function runHook(
  input: Record<string, unknown>,
  env: Record<string, string> & { readonly HOME: string },
  options: { readonly preload?: string | readonly string[] } = {},
): Promise<{ readonly exitCode: number; readonly stdout: string; readonly stderr: string }> {
  const preloading = [options.preload ?? []].flat().flatMap((file) => ["--preload", file])
  const run = Bun.spawn(["bun", ...preloading, hookEntry, "open-asks"], {
    env: { PATH: process.env["PATH"] ?? "", ...NO_TRANSPILER_CACHE, ...env },
    stdin: new TextEncoder().encode(JSON.stringify({ cwd: env.HOME, ...input })),
    stdout: "pipe",
    stderr: "pipe",
  })
  const stdout = collected(run.stdout)
  const stderr = collected(run.stderr)
  const exitCode = await exitBeforeTheGuard(
    run,
    "the open-asks hook",
    () =>
      `it wrote ${JSON.stringify(stdout.text())} to stdout, ${JSON.stringify(stderr.text())} to stderr`,
  )
  return { exitCode, stdout: await stdout.ended, stderr: await stderr.ended }
}

const hookInput = {
  session_id: "s-1",
  source: "startup",
  hook_event_name: "SessionStart",
}

test("summarises the session's open asks into context and records the session id for its Claude process", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-"))
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(
        `${request.headers.get("authorization")} ${new URL(request.url).pathname}?${new URL(request.url).searchParams}`,
      )
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 1,
        waiting_on_human: 1,
        waiting_on_agent: 0,
        asks: [
          {
            id: "ask-9",
            ref: "/issues/LEGION-9#ask-9",
            question: "Ship the bundle?",
            kind: "decision",
            urgency: "med",
            created_at: "2026-09-14T00:00:00Z",
            age_seconds: 90,
            priority: 1,
            owner: { issue: { key: "LEGION-9", title: "Bundle" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          },
        ],
      })
    },
  })
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN,
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith(
      "Dispatch authored-ask summary:\n1 unanswered ask you authored",
    )
    expect(result.stdout).toContain("Ship the bundle?")
    expect(requests).toEqual([`Bearer ${DISPATCH_TOKEN} /api/v1/asks/open?author_session=s-1`])
    expect(
      await readFile(
        join(scratch, "plugin-data", "sessions", String(process.pid), "session-id"),
        "utf8",
      ),
    ).toBe("s-1\n")
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// Each hook names itself by its program and the release of the package its harness runs: Claude
// Code's session hooks as claude-envoy-hooks, Codex's session hook (`hooks/codex-hook.ts`, whose
// session-start reads the open asks) as codex-envoy-hooks, at `@sjawhar/codex-envoy`'s release.
// The Codex hook is started as `runHook` starts Claude Code's: its transpiler cache off and its
// exit waited on until the row's guard (`exitBeforeTheGuard`).
test.each([
  [
    "claude-envoy-hooks",
    packageVersion,
    (env: Record<string, string> & { readonly HOME: string }) => runHook(hookInput, env),
  ],
  [
    "codex-envoy-hooks",
    codexPackageVersion,
    async (env: Record<string, string> & { readonly HOME: string }) => {
      const run = Bun.spawn(
        ["bun", resolve(import.meta.dir, "..", "hooks", "codex-hook.ts"), "session-start"],
        {
          env: { PATH: process.env["PATH"] ?? "", ...NO_TRANSPILER_CACHE, ...env },
          stdin: new TextEncoder().encode(JSON.stringify({ session_id: "s-1", cwd: env.HOME })),
          stdout: "pipe",
          stderr: "pipe",
        },
      )
      const stdout = collected(run.stdout)
      const stderr = collected(run.stderr)
      const exitCode = await exitBeforeTheGuard(
        run,
        "the Codex session-start hook",
        () =>
          `it wrote ${JSON.stringify(stdout.text())} to stdout, ${JSON.stringify(stderr.text())} to stderr`,
      )
      return { exitCode, stdout: await stdout.ended, stderr: await stderr.ended }
    },
  ],
] as const)("names itself to Dispatch as %s and its release", async (program, version, run) => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-agent-"))
  const sent: { readonly path: string; readonly userAgent: string | null }[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      sent.push({
        path: new URL(request.url).pathname,
        userAgent: request.headers.get("user-agent"),
      })
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      })
    },
  })
  try {
    const result = await run({
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN,
    })

    expect(result.exitCode).toBe(0)
    expect(sent).toEqual([{ path: "/api/v1/asks/open", userAgent: `${program}/${version}` }])
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// The hook ends its process once its output is out (`endOnceWritten`, session-hook.ts): a summary
// far larger than a pipe holds (64 KiB on Linux), read slowly, must still arrive whole, its last ask
// included, so the exit waits until stdout has taken every byte. Read this way, a 2 MB write to a
// pipe lost its tail on 10 of 10 exits that did not end the stream first, and on none that did.
test("a summary larger than a pipe holds arrives whole before the hook ends its process", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-large-"))
  const asks = Array.from({ length: 2_000 }, (_, index) => ({
    id: `ask-${index}`,
    ref: `/issues/LEGION-9#ask-${index}`,
    question: `Ship part ${index} of the large summary, ${"long ".repeat(200)}?`,
    kind: "decision",
    urgency: "med",
    created_at: "2026-09-14T00:00:00Z",
    age_seconds: 90,
    priority: 1,
    owner: { issue: { key: "LEGION-9", title: "Bundle" } },
    human_replied: false,
    last_reply: null,
    waiting_on: "human",
  }))
  const server = Bun.serve({
    port: 0,
    fetch: () =>
      Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: asks.length,
        waiting_on_human: asks.length,
        waiting_on_agent: 0,
        asks,
      }),
  })
  try {
    const run = Bun.spawn(["bun", hookEntry, "open-asks"], {
      env: {
        PATH: process.env["PATH"] ?? "",
        ...NO_TRANSPILER_CACHE,
        HOME: scratch,
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
        DISPATCH_URL: `http://127.0.0.1:${server.port}`,
        DISPATCH_TOKEN,
      },
      stdin: new TextEncoder().encode(JSON.stringify({ cwd: scratch, ...hookInput })),
      stdout: "pipe",
      stderr: "pipe",
    })
    const stderr = collected(run.stderr)
    // Read slowly, a pause after each chunk, so the hook's writes outrun the pipe.
    const read = (async () => {
      let text = ""
      const decoder = new TextDecoder()
      for await (const chunk of run.stdout) {
        text += decoder.decode(chunk, { stream: true })
        await Bun.sleep(5)
      }
      return text + decoder.decode()
    })()
    const exitCode = await exitBeforeTheGuard(run, "the open-asks hook", () => stderr.text())
    const stdout = await read

    expect(exitCode).toBe(0)
    expect(Buffer.byteLength(stdout)).toBeGreaterThan(2_000_000)
    expect(stdout.match(/Ship part \d+ of the large summary/g)?.length).toBe(asks.length)
    expect(stdout).toContain("Ship part 1999 of the large summary")
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

test("reports an unreachable Dispatch as unavailable, still exits 0, and still records the session id", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-down-"))
  const closed = Bun.serve({ port: 0, fetch: () => new Response() })
  const port = closed.port
  closed.stop(true)
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_URL: `http://127.0.0.1:${port}`,
      DISPATCH_TOKEN,
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith("Dispatch authored-ask summary unavailable: ")
    expect(
      await readFile(
        join(scratch, "plugin-data", "sessions", String(process.pid), "session-id"),
        "utf8",
      ),
    ).toBe("s-1\n")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// A password holding an unencoded '@' names no host, and the URL parser reads the password's tail
// as one, which the runtime's resolver error repeats. The line reaches the model's context, so the
// Dispatch client names the failure by its class and the URL by its fixed phrase (#4140, B1). The
// hook's fetch of that `.invalid` host is answered by a stand-in that fails as Bun's resolver does
// (`unresolved-names.ts`), so the row asks no DNS server and does not wait on its answer.
test("names an unreachable Dispatch whose password holds an '@' by no part of the password", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-at-"))
  const tail = `Tail${crypto.randomUUID().slice(0, 8)}`
  try {
    const names = await unresolvedNames(scratch)
    const result = await runHook(
      hookInput,
      {
        HOME: scratch,
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
        DISPATCH_URL: `https://user-7f2a:p@ssword${tail}.invalid`,
        DISPATCH_TOKEN,
      },
      { preload: names.preload },
    )

    // The hook's error named the password's tail, so the checks below can fail.
    expect(await names.asked()).toEqual([`ssword${tail}.invalid`.toLowerCase()])
    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith(
      "Dispatch authored-ask summary unavailable: Dispatch at the configured Dispatch URL (not printed: it carries a credential or is not a plain http(s) URL) is unreachable: ",
    )
    for (const text of [result.stdout, result.stderr]) {
      expect(text.toLowerCase()).not.toContain(tail.toLowerCase())
    }
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("a SessionStart carrying a session title records it, and one carrying none removes it", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-title-"))
  const pluginData = join(scratch, "plugin-data")
  const titleFile = join(pluginData, "titles", "s-1.txt")
  try {
    const named = await runHook(
      { ...hookInput, session_title: "review the listener" },
      { HOME: scratch, CLAUDE_PLUGIN_DATA: pluginData },
    )

    expect(named.exitCode).toBe(0)
    expect(await readFile(titleFile, "utf8")).toBe("review the listener\n")

    // The user undid the rename: Claude Code sends no session_title, and the record goes.
    const unnamed = await runHook(hookInput, { HOME: scratch, CLAUDE_PLUGIN_DATA: pluginData })

    expect(unnamed.exitCode).toBe(0)
    expect(await readdir(join(pluginData, "titles"))).toEqual([])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("adds nothing to context when Dispatch is not configured", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-off-"))
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe("")
    expect(
      await readFile(
        join(scratch, "plugin-data", "sessions", String(process.pid), "session-id"),
        "utf8",
      ),
    ).toBe("s-1\n")
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("summarises a missing Claude profile file with where its unread old file belongs", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-profile-off-"))
  const configDirectory = join(scratch, "claude-config")
  const old = join(scratch, ".config", "opencode", "envoy.json")
  await mkdir(configDirectory)
  await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: "old-token",
      },
    }),
  )
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    })
    const chosen = join(configDirectory, "envoy.json")

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe(
      `Dispatch authored-ask summary unavailable: no envoy.json at ${chosen}: Dispatch is off; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// A token file left without DISPATCH_URL names the lost variable, and the unread old file's advice
// still ends the notice, which is what the hook looks for before it says anything.
test("a DISPATCH_TOKEN_FILE with no DISPATCH_URL names the lost URL and still where the old file belongs", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-lost-url-"))
  const configDirectory = join(scratch, "claude-config")
  const old = join(scratch, ".config", "opencode", "envoy.json")
  const tokenFile = join(scratch, "DISPATCH_TOKEN")
  await mkdir(configDirectory)
  await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
  await writeFile(old, JSON.stringify({ dispatch: { serverUrl: "https://old.dispatch.example" } }))
  await writeFile(tokenFile, `${fixtureSecret("pointer")}\n`)
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      DISPATCH_TOKEN_FILE: tokenFile,
    })
    const chosen = join(configDirectory, "envoy.json")

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe(
      `Dispatch authored-ask summary unavailable: DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is for; ${old} is not read for this profile; copy or link it to ${chosen}, or write that file\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("says nothing of the unread old user file when a checkout's enabled: false turns Dispatch off", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-checkout-off-"))
  const checkout = join(scratch, "checkout")
  const old = join(scratch, ".config", "opencode", "envoy.json")
  await mkdir(join(checkout, ".envoy"), { recursive: true })
  await writeFile(
    join(checkout, ".envoy", "envoy.json"),
    JSON.stringify({ dispatch: { enabled: false } }),
  )
  await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: "https://old.dispatch.example",
        token: "old-token",
      },
    }),
  )
  try {
    // No ~/.config/legion/envoy.json, so the read names the old file beside the missing one; the
    // checkout's own enabled: false, not the missing file, is what turns Dispatch off.
    const result = await runHook({ ...hookInput, cwd: checkout }, { HOME: scratch })
    expect(result.exitCode).toBe(0)
    expect(result.stdout).toBe("")

    // The same session without the old file prints the same.
    await rm(old)
    const withoutOld = await runHook({ ...hookInput, cwd: checkout }, { HOME: scratch })
    expect(withoutOld.exitCode).toBe(0)
    expect(withoutOld.stdout).toBe(result.stdout)
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("names a checkout's unread .opencode/envoy.json and still summarises the asks from the user file's server", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-old-checkout-"))
  const checkout = join(scratch, "checkout")
  const old = join(checkout, ".opencode", "envoy.json")
  /** The token in the checkout's old file, which the hook names and never reads. */
  const checkoutToken = fixtureSecret("dsp")
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(`${request.headers.get("authorization")} ${new URL(request.url).pathname}`)
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      })
    },
  })
  await mkdir(join(scratch, ".config", "legion"), { recursive: true })
  await writeFile(
    join(scratch, ".config", "legion", "envoy.json"),
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: `http://127.0.0.1:${server.port}`,
        token: DISPATCH_TOKEN,
      },
    }),
  )
  await mkdir(join(checkout, ".opencode"), { recursive: true })
  await writeFile(
    old,
    JSON.stringify({
      dispatch: { enabled: true, serverUrl: "http://127.0.0.1:1", token: checkoutToken },
    }),
  )
  const rename = `envoy: ${old} is not read: mkdir -p ${join(checkout, ".envoy")} && mv ${old} ${join(checkout, ".envoy", "envoy.json")}\n`
  try {
    const result = await runHook(
      { ...hookInput, cwd: checkout },
      { HOME: scratch, CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data") },
    )

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith(`${rename}Dispatch authored-ask summary:\n`)
    expect(result.stdout.split(rename)).toHaveLength(2)
    expect(requests).toEqual([`Bearer ${DISPATCH_TOKEN} /api/v1/asks/open`])

    // With no user file the checkout's old file is all that ever configured Dispatch here: the
    // hook still names it, beside the missing file's quiet off.
    await rm(join(scratch, ".config", "legion", "envoy.json"))
    const off = await runHook(
      { ...hookInput, cwd: checkout },
      { HOME: scratch, CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data") },
    )
    expect(off.exitCode).toBe(0)
    expect(off.stdout).toBe(rename)
    expect(requests).toHaveLength(1)
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// The `dispatch` command the plugin's bin/ puts on the Bash tool's PATH reads its host from
// DISPATCH_HOST, and its envoy.json by Claude Code's rule: CLAUDE_CONFIG_DIR's, else the file
// ENVOY_CONFIG_FILE names; Claude Code sources CLAUDE_ENV_FILE into every later Bash command of the
// session.
test("names Claude Code as the dispatch host and hands on the session's envoy.json in CLAUDE_ENV_FILE, after whatever other hooks wrote", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-env-"))
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
    })

    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export FROM_ANOTHER_HOOK=1",
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("hands on the CLAUDE_CONFIG_DIR envoy.json, single-quoted so the shell reads the path as written", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-config-"))
  const configDirectory = join(scratch, "it's claude")
  await mkdir(configDirectory)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_CONFIG_DIR: configDirectory,
      CLAUDE_ENV_FILE: envFile,
    })

    expect(result.exitCode).toBe(0)
    const file = join(configDirectory, "envoy.json")
    expect(await readFile(envFile, "utf8")).toBe(
      `export DISPATCH_HOST=claude\nexport ENVOY_CONFIG_FILE='${file.replaceAll("'", "'\\''")}'\n`,
    )
    const sourced = Bun.spawnSync([
      "bash",
      "-c",
      `. "$1"; printf %s "$ENVOY_CONFIG_FILE"`,
      "-",
      envFile,
    ])
    expect(sourced.stdout.toString()).toBe(file)
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// Swap the hook's harness for the `command` rule, or omp's, and it would hand on, and read open
// asks from, a file an outer session or launcher left in the environment.
test("reads and hands on Claude Code's own envoy.json, never the ENVOY_CONFIG_FILE or omp profile it inherited", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-harness-"))
  const requests: string[] = []
  const server = Bun.serve({
    port: 0,
    fetch: (request) => {
      const url = new URL(request.url)
      requests.push(`${request.headers.get("authorization")} ${url.pathname}?${url.searchParams}`)
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      })
    },
  })
  const configDirectory = join(scratch, "claude-config")
  await mkdir(configDirectory)
  await writeFile(
    join(configDirectory, "envoy.json"),
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: `http://127.0.0.1:${server.port}`,
        token: DISPATCH_TOKEN,
      },
    }),
  )
  const decoy = join(scratch, "decoy")
  await mkdir(decoy)
  await writeFile(join(decoy, "envoy.json"), "{not json")
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      CLAUDE_CONFIG_DIR: configDirectory,
      ENVOY_CONFIG_FILE: join(decoy, "envoy.json"),
      OMP_PROFILE: "work",
      PI_CODING_AGENT_DIR: decoy,
    })

    expect(result.exitCode).toBe(0)
    expect(requests).toEqual([`Bearer ${DISPATCH_TOKEN} /api/v1/asks/open?author_session=s-1`])
    expect(await readFile(envFile, "utf8")).toContain(
      `export ENVOY_CONFIG_FILE='${join(configDirectory, "envoy.json")}'`,
    )
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

test("with a relative XDG_CONFIG_HOME, hands on no envoy.json, says why Dispatch is unavailable, and still exits 0", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-xdg-"))
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      XDG_CONFIG_HOME: "relative/config",
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
    })

    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toBe("export DISPATCH_HOST=claude\n")
    expect(result.stdout).toBe(
      "Dispatch authored-ask summary unavailable: XDG_CONFIG_HOME is relative/config, which is not an absolute path\n",
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// SessionStart fires on startup, resume, clear, compact and fork; the file keeps one of each line.
test("writes the host and envoy.json lines once however many SessionStarts write the same env file", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-env-twice-"))
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const env = {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
    }
    expect((await runHook(hookInput, env)).exitCode).toBe(0)
    expect((await runHook(hookInput, env)).exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export FROM_ANOTHER_HOOK=1",
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("writes no environment line anywhere when Claude Code names no env file", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-no-env-"))
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).not.toContain("DISPATCH_HOST")
    expect(result.stdout).not.toContain("ENVOY_CONFIG_FILE")
    expect(await readFile(envFile, "utf8")).toBe("export FROM_ANOTHER_HOOK=1\n")
    // Only the session record the hook keeps for its Claude process is written (beside the cache
    // Bun itself keeps under HOME).
    const written = (await readdir(scratch)).filter((entry) => entry !== ".bun").sort()
    expect(written).toEqual(["plugin-data", "session-env.sh"])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

/** A directory holding a `jj` that runs `body`, to put first on the hook's PATH: the hook writes
 * the pane's attribution overlay over the config chain `jj config path --user` prints. */
async function jjDirectory(scratch: string, body: string): Promise<string> {
  const bin = join(scratch, "bin")
  await mkdir(bin, { recursive: true })
  const jj = join(bin, "jj")
  await writeFile(jj, `#!/bin/sh\n${body}\n`)
  await chmod(jj, 0o755)
  return bin
}

/** A Dispatch answering one empty open-asks snapshot, so a test can see the summary the hook
 * still prints beside whatever it wrote to the env file. */
function openAsksServer() {
  return Bun.serve({
    port: 0,
    fetch: () =>
      Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 0,
        waiting_on_human: 0,
        waiting_on_agent: 0,
        asks: [],
      }),
  })
}

/** The overlay's unscoped `[templates]` table for `hookInput`'s session, written out line by line:
 * the session line, then the role line when `role` is given, in each of the three templates. */
function unscopedTable(role?: string): string {
  const withRole = role !== undefined
  return [
    "[templates]",
    `commit_trailers = '"Omp-Session: s-1${withRole ? `\\nLegion-Role: ${role}` : ""}"'`,
    "revert_description = '''",
    "concat(",
    `  'Revert "' ++ description.first_line() ++ '"' ++ "\\n",`,
    `  "\\n",`,
    `  "This reverts commit " ++ commit_id ++ ".\\n",`,
    `  "\\n",`,
    `  "Omp-Session: s-1\\n",`,
    ...(withRole ? [`  "Legion-Role: ${role}\\n",`] : []),
    ")",
    "'''",
    "duplicate_description = '''",
    "concat(",
    "  description,",
    "  if(description, concat(",
    `    if(!trailers, "\\n"),`,
    `    if(!trailers.any(|t| t.key() == "Omp-Session" && t.value() == "s-1"), "Omp-Session: s-1\\n"),`,
    ...(withRole
      ? [
          `    if(!trailers.any(|t| t.key() == "Legion-Role" && t.value() == "${role}"), "Legion-Role: ${role}\\n"),`,
        ]
      : []),
    "  )),",
    ")",
    "'''",
    "",
  ].join("\n")
}

// A Legion pane's jj commits carry the pane's session, its role and its issue, through an overlay
// the hook writes and the JJ_CONFIG every later Bash command of the session sources. The hook
// writes it because the pane's claim boot cannot: that boot is the MCP server Claude Code spawns
// as a child, and a child cannot change its parent's environment. The issue line is scoped to the
// pane's own workspace, so a commit the same shell makes in another repository carries no issue
// line. This pane has no LEGION_ROLE, so both tables carry the session line alone.
test("writes the pane's attribution overlay and exports the chain layering it over jj's own config", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const workspace = join(scratch, "workspace")
  await mkdir(workspace)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
      LEGION_WORKSPACE: workspace,
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        `export JJ_CONFIG='${userConfig}:${overlay}'`,
        "",
      ].join("\n"),
    )
    expect(await readFile(overlay, "utf8")).toBe(
      unscopedTable() +
        `\n[[--scope]]\n--when.workspaces = ["${await realpath(workspace)}"]\n` +
        `[--scope.templates]\ncommit_trailers = '"Omp-Session: s-1\\nDispatch: ACME-7"'\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// A pane whose LEGION_ROLE names a role gets the role line after the session line. The scoped
// table carries it too, before the issue line: a scoped commit_trailers replaces the unscoped one.
test("writes the pane's role after the session line, in the unscoped table and the scoped one", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-role-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const workspace = join(scratch, "workspace")
  await mkdir(workspace)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
      LEGION_WORKSPACE: workspace,
      LEGION_ROLE: "implementer",
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    expect(await readFile(envFile, "utf8")).toContain(
      `export JJ_CONFIG='${userConfig}:${overlay}'\n`,
    )
    expect(await readFile(overlay, "utf8")).toBe(
      unscopedTable("implementer") +
        `\n[[--scope]]\n--when.workspaces = ["${await realpath(workspace)}"]\n[--scope.templates]\n` +
        `commit_trailers = '"Omp-Session: s-1\\nLegion-Role: implementer\\nDispatch: ACME-7"'\n`,
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// Without a workspace there is nothing to scope the key to, and an unscoped key would reach every
// jj repository the pane's shell commits in.
test("writes the session line alone when the pane names no workspace", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-no-ws-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
    })

    expect(result.exitCode).toBe(0)
    expect(await readFile(join(stateDir, "omp-attribution-s-1.toml"), "utf8")).toBe(unscopedTable())
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

test("writes the session line alone when the daemon named no issue key", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-no-key-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    expect(await readFile(envFile, "utf8")).toContain(
      `export JJ_CONFIG='${userConfig}:${overlay}'\n`,
    )
    expect(await readFile(overlay, "utf8")).toBe(unscopedTable())
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// A Claude Code session that is no Legion pane commits as whoever runs it: no overlay, and no
// JJ_CONFIG to replace whatever jj config that person's shell already has.
test("writes no overlay and no JJ_CONFIG for a session that is no Legion pane", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-off-"))
  const bin = await jjDirectory(
    scratch,
    `printf '%s\\n' ${JSON.stringify(join(scratch, "jj-user.toml"))}`,
  )
  const envFile = join(scratch, "session-env.sh")
  const server = openAsksServer()
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN,
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith("Dispatch authored-ask summary:\n")
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
    // No overlay was written anywhere: the only directory one could reach is the state directory
    // the session has none of (beside the cache Bun itself keeps under HOME).
    const written = (await readdir(scratch)).filter((entry) => entry !== ".bun").sort()
    expect(written).toEqual(["bin", "plugin-data", "session-env.sh"])
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// Claude Code gives each session id its own env file, so the runs that find a line already there
// are a resume and a compaction of the same id, which this hook runs again over the file that id
// wrote; the chain it resolves can differ between them. Two export lines in one sourced file
// would leave which chain wins to the order they were written in.
test("keeps one JJ_CONFIG export, naming the chain this run resolved, when the same id runs again", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-twice-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const laterConfig = join(scratch, "jj-user-later.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const laterBin = await jjDirectory(
    join(scratch, "later"),
    `printf '%s\\n' ${JSON.stringify(laterConfig)}`,
  )
  const envFile = join(scratch, "session-env.sh")
  await writeFile(envFile, "export FROM_ANOTHER_HOOK=1\n")
  try {
    const env = {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
    }
    expect((await runHook(hookInput, env)).exitCode).toBe(0)
    expect(
      (await runHook(hookInput, { ...env, PATH: `${laterBin}:${process.env["PATH"] ?? ""}` }))
        .exitCode,
    ).toBe(0)

    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export FROM_ANOTHER_HOOK=1",
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        `export JJ_CONFIG='${laterConfig}:${join(stateDir, `omp-attribution-${hookInput.session_id}.toml`)}'`,
        "",
      ].join("\n"),
    )
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// An unquoted path holding a space or a quote would break every Bash command of the session.
test("single-quotes the chain, so a shell sourcing the file reads a state directory path whole", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-quoted-"))
  const stateDir = join(scratch, "it's a state dir")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const bin = await jjDirectory(scratch, `printf '%s\\n' ${JSON.stringify(userConfig)}`)
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
    })

    expect(result.exitCode).toBe(0)
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    const sourced = Bun.spawnSync(["bash", "-c", `. "$1"; printf %s "$JJ_CONFIG"`, "-", envFile])
    expect(sourced.stdout.toString()).toBe(`${userConfig}:${overlay}`)
    expect(await readFile(overlay, "utf8")).toBe(unscopedTable())
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// Attribution is worth nothing the session's own start: a jj that never answers costs the pane the
// export, and neither the two lines the `dispatch` command needs nor the summary. This jj answers,
// but only at four times the hook's wait for it (`JJ_BASE_CHAIN_TIMEOUT_MS`, 12 s), inside the
// row's guard: a hook that waits on jj until it answers writes its chain into the overlay and the
// env file, and what the files hold decides the row, not how long the hook took. So this row holds
// only that the hook does not wait for jj to answer: a hook that waits past its bound but gives up
// before four times it passes here, and fails the census rows after the budget row below, which
// hold the hook's jj wait to exactly its bound.
test("gives up on a jj that never answers, keeping the other exports and the summary", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-hangs-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const late = join(scratch, "jj-user.toml")
  const answersAfter = (4 * JJ_BASE_CHAIN_TIMEOUT_MS) / 1000
  const bin = await jjDirectory(
    scratch,
    `sleep ${answersAfter}; printf '%s\\n' ${JSON.stringify(late)}`,
  )
  const envFile = join(scratch, "session-env.sh")
  const server = openAsksServer()
  try {
    const result = await runHook(hookInput, {
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
      DISPATCH_URL: `http://127.0.0.1:${server.port}`,
      DISPATCH_TOKEN,
    })

    expect(result.exitCode).toBe(0)
    expect(result.stdout).toStartWith("Dispatch authored-ask summary:\n")
    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        "",
      ].join("\n"),
    )
    expect(await readdir(stateDir)).toEqual([])
  } finally {
    server.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// `CLAUDE_ENV_FILE` is one file per session id
// (`<config>/session-env/<session id>/sessionstart-hook-<n>.sh` on the pinned Claude Code
// 2.1.311), so the only runs that find an `export JJ_CONFIG=` line already written are a resume
// or a compaction of that same id — which the Legion daemon does on every relaunch. The line
// there names this same session's own overlay, still on disk: a jj that never answers must leave
// it, since removing it would leave the session with no trailer. The writer takes its base
// from an inherited JJ_CONFIG and spawns no jj at all when it finds one, so both runs are given
// an environment carrying none, as Claude Code's hook process is.
test("keeps the session's own export when a later run of the same id has no jj to answer", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-jj-resume-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const userConfig = join(scratch, "jj-user.toml")
  const envFile = join(scratch, "session-env.sh")
  try {
    const env = (bin: string) => ({
      HOME: scratch,
      PATH: `${bin}:${process.env["PATH"] ?? ""}`,
      CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
      CLAUDE_ENV_FILE: envFile,
      LEGION_STATE_DIR: stateDir,
      LEGION_ISSUE_TRAILER: "ACME-7",
    })
    const overlay = join(stateDir, "omp-attribution-s-1.toml")
    const working = await jjDirectory(
      join(scratch, "working"),
      `printf '%s\\n' ${JSON.stringify(userConfig)}`,
    )
    expect((await runHook(hookInput, env(working))).exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toContain(
      `export JJ_CONFIG='${userConfig}:${overlay}'\n`,
    )

    // The resume: same session id, same file, and a jj that never answers.
    const hanging = await jjDirectory(join(scratch, "hanging"), "exec sleep 30")
    expect((await runHook(hookInput, env(hanging))).exitCode).toBe(0)

    expect(await readFile(envFile, "utf8")).toBe(
      [
        "export DISPATCH_HOST=claude",
        `export ENVOY_CONFIG_FILE='${scratch}/.config/legion/envoy.json'`,
        `export JJ_CONFIG='${userConfig}:${overlay}'`,
        "",
      ].join("\n"),
    )
    expect(await readdir(stateDir)).toEqual(["omp-attribution-s-1.toml"])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

/** What the row below leaves for Bun to start the hook before its first wait. A start of
 *  hooks/session-hook.ts in an empty HOME took 186 to 450 ms (median 293 ms) with the cache
 *  written, and 141 to 274 ms without it, in 9 runs each at a load of about 600 (2026-11-22): this
 *  is four times the slowest, rounded up. */
const HOOK_START_ALLOWANCE_MS = 2_000

// Claude Code kills the open-asks hook at the timeout hooks.json gives it, and the session then
// loses its exports and its summary. After Bun's start-up the hook runs three waits one after the
// other: jj's config chain, the resolution of Dispatch (a `!` dispatch.token command runs for up
// to `COMMAND_TIMEOUT_MS`), and the Dispatch read. Their bounds and the start-up must end inside
// that timeout together: a fact of the constants and the file, which no clock of this machine's
// could show. The rows below hold that the hook's own waits are these and run to these bounds.
test("the hook's start-up and its jj, token command and Dispatch bounds end inside the timeout hooks.json gives open-asks", async () => {
  const config = JSON.parse(
    await readFile(resolve(import.meta.dir, "..", "hooks", "hooks.json"), "utf8"),
  ) as { hooks: Record<string, { hooks: { args?: string[]; timeout?: number }[] }[]> }
  const timeouts = Object.values(config.hooks)
    .flat()
    .flatMap((group) => group.hooks)
    .filter((hook) => hook.args?.at(-1) === "open-asks")
    .map((hook) => hook.timeout)
  expect(timeouts).toHaveLength(1)
  const spent = {
    startUp: HOOK_START_ALLOWANCE_MS,
    jj: JJ_BASE_CHAIN_TIMEOUT_MS,
    tokenCommand: COMMAND_TIMEOUT_MS,
    dispatchRead: OPEN_ASKS_TIMEOUT_MS,
  }
  const total = Object.values(spent).reduce((sum, ms) => sum + ms, 0)
  expect(total, JSON.stringify(spent)).toBeLessThan((timeouts[0] ?? 0) * 1000)
})

/** The census of the hook's timed waits and starts, preloaded into its process. */
const timedWaitsPreload = resolve(import.meta.dir, "hook-timed-waits.ts")
/** The repository's root, which the census's and the scan's file names are given against. */
const repositoryRoot = resolve(import.meta.dir, "..", "..", "..")

/** A wait or start the census must hold: the file of the hook's code that made it, its kind, the
 *  program it started, and its declared bound (-1 for a start with none). A bound computed from
 *  another wait's time left is given as `atMost` with that wait as `inside`: the entry must be
 *  made, and end, inside that wait's declared span; a bound drawn at random (a retry's jitter) is
 *  given as `atMost` alone. A wait the hook's exit must cut, background work the hook does not wait
 *  for, is marked `cut`: it must not have run to its bound. */
type CensusEntry = {
  readonly file: string
  readonly kind: string
  readonly command?: string
  readonly cut?: true
} & ({ readonly ms: number } | { readonly atMost: number; readonly inside?: CensusEntry })

/** `wait`'s file, from the frame of the hook's code that made it, against the repository's root. */
function madeIn(wait: TimedWait): string {
  const path = /\(?(\/[^():]+):\d+:\d+\)?$/.exec(wait.at)?.[1]
  if (path === undefined) return wait.at
  // Real paths on both sides, so a checkout reached through a link, or a node_modules that is one,
  // names each file as the scan and the expected entries do.
  let real = path
  try {
    real = realpathSync(path)
  } catch {
    // A frame's file that is gone by now keeps the path the frame gave.
  }
  return relative(realpathSync(repositoryRoot), real)
}

/** The key an expected entry and a census entry are compared by. */
function censusKey(entry: CensusEntry): string {
  const bound =
    "ms" in entry
      ? `${entry.ms} ms`
      : `at most ${entry.atMost} ms${entry.inside === undefined ? "" : ` inside (${censusKey(entry.inside)})`}`
  const cut = entry.cut === true ? ", cut at the exit" : ""
  return `${entry.kind}${entry.command === undefined ? "" : ` ${entry.command}`} ${bound} in ${entry.file}${cut}`
}

/**
 * Runs the hook in `scratch` with `env` and the census preloaded (`hook-timed-waits.ts`), and
 * fails unless the census is exactly `expected`: every process the hook started, and every timed
 * wait it made with a bound above zero, whether it ran to its bound or ended sooner, by file,
 * kind, program and bound. One more start or wait of any length, or one with another bound, fails
 * the row by name, and so does a census that is missing. A zero-length timer is a turn of the
 * event loop and is left out. Returns what the hook printed, for the row to check that it met each
 * wait.
 */
async function runToItsTimedWaits(
  scratch: string,
  env: Record<string, string> & { readonly HOME: string },
  expected: readonly CensusEntry[],
  ceilings: {
    readonly zeroLengthTimers: number
    readonly clockReads: number
    readonly cpuMs: number
  },
  preloads: readonly string[] = [],
  input: Record<string, unknown> = hookInput,
): Promise<{
  readonly exitCode: number
  readonly stdout: string
  readonly stderr: string
  readonly census: Census
}> {
  const record = join(scratch, "timed-waits.json")
  const result = await runHook(
    input,
    { ...env, HOOK_TIMED_WAITS: record },
    { preload: [timedWaitsPreload, ...preloads] },
  )
  const recorded = await readFile(record, "utf8").catch((error: unknown) => {
    throw new Error(
      `the hook, exit ${result.exitCode}, wrote no census of its timed waits, so nothing says what ` +
        `it waited on (${String(error)}): ${result.stderr}`,
    )
  })
  // Written by this package's own preload in the hook's process, a step ago.
  const census: Census = JSON.parse(recorded)
  // Every entry but a zero-length one: a start, or a wait with no bound (-1), counts too.
  const counted = census.waits.filter((wait) => wait.ms !== 0)
  const key = (wait: TimedWait): string => {
    const plain = {
      file: madeIn(wait),
      kind: wait.kind,
      ms: wait.ms,
      ...(wait.command === undefined ? {} : { command: wait.command }),
    }
    for (const derived of expected) {
      if (
        "ms" in derived ||
        derived.file !== plain.file ||
        derived.kind !== plain.kind ||
        wait.ms > derived.atMost
      )
        continue
      const inside = derived.inside
      const parent =
        inside === undefined ? undefined : counted.find((other) => key(other) === censusKey(inside))
      if (
        inside === undefined ||
        (parent !== undefined &&
          parent.armedAt <= wait.armedAt &&
          wait.armedAt + wait.ms <= parent.armedAt + parent.ms)
      ) {
        const { cut, ...bare } = derived
        return censusKey(cut === true && wait.outcome !== "fired" ? { ...bare, cut } : bare)
      }
    }
    // A wait the row expects cut keeps the mark only while it did not run to its bound.
    const cut = expected.some(
      ({ cut, ...bare }) => cut === true && censusKey(bare) === censusKey(plain),
    )
    return censusKey(cut && wait.outcome !== "fired" ? { ...plain, cut: true as const } : plain)
  }
  expect(
    counted.map(key).sort(),
    `the hook's starts and timed waits: ${counted.map((wait) => `${wait.kind}${wait.command === undefined ? "" : ` ${wait.command}`} ${wait.ms} ms, ${wait.outcome}, at ${wait.at}`).join("; ")}`,
  ).toEqual(expected.map(censusKey).sort())
  // A wait made of turns of the event loop or of a loop on the clock arms no timer above zero:
  // the counts show it, far above what the path makes. Each ceiling is the path's own count with
  // room for a few more reads, so no wait of more than a few turns passes.
  // CPU work arms nothing and may read no clock: the CPU time the hook had used at its exit shows
  // it, held under a bound far over what the path uses, which a loaded machine barely moves
  // (`HOOK_CPU_CEILING_MS`).
  expect(
    {
      zeroLengthTimers: census.zeroLengthTimers,
      clockReads: census.clockReads,
      cpuMs: census.cpuMs,
    },
    `the hook's zero-length timers, clock reads and CPU time, against the ceilings ${JSON.stringify(ceilings)}`,
  ).toEqual({
    zeroLengthTimers: Math.min(census.zeroLengthTimers, ceilings.zeroLengthTimers),
    clockReads: Math.min(census.clockReads, ceilings.clockReads),
    cpuMs: Math.min(census.cpuMs, ceilings.cpuMs),
  })
  return { ...result, census }
}

/** The jj start and its wait, which every row below makes the hook run to its bound. */
const JJ_START: CensusEntry = {
  file: "packages/pi-shared/src/jj-attribution.ts",
  kind: "Bun.spawn",
  command: "jj",
  ms: -1,
}
const JJ_WAIT: CensusEntry = {
  file: "packages/pi-shared/src/jj-attribution.ts",
  kind: "setTimeout",
  ms: JJ_BASE_CHAIN_TIMEOUT_MS,
}
/**
 * The CPU time a census row lets the hook use, over all its threads, user and system together: far
 * over what any path but the pool's used, 204 to 527 ms in every census row, each run with no load
 * added (box load 960 to 1,770), on 2 CPUs with 8 busy loops, and with only the hook starved, so a
 * hook that does seconds of work no timer or clock read shows (Tester4955's m9, a loop of 8e9
 * turns, about 14 s) fails. CPU time, unlike the time a row takes, a loaded machine barely moves:
 * the same rows used no more pinned or starved than with no load added.
 */
const HOOK_CPU_CEILING_MS = 2_500
/** The pool route's: its CPU varied from 358 to 2,305 ms with no tie to load (2,305 with no load
 *  added, 659 pinned, 358 starved), about four times its highest. */
const POOL_CPU_CEILING_MS = 10_000

/** Each census row's ceilings on zero-length timers and clock reads: the path's own counts (no
 *  zero-length timer; 1, 4 and 4 clock reads), the same in every run measured, idle, on 2 loaded
 *  CPUs and starved, with room for four more of each. A wait of five turns or five reads fails. */
const COMMAND_ROW_CEILINGS = { zeroLengthTimers: 4, clockReads: 5, cpuMs: HOOK_CPU_CEILING_MS }
const READ_ROW_CEILINGS = { zeroLengthTimers: 4, clockReads: 8, cpuMs: HOOK_CPU_CEILING_MS }
const SUCCESS_ROW_CEILINGS = { zeroLengthTimers: 4, clockReads: 8, cpuMs: HOOK_CPU_CEILING_MS }
/** The Dispatch read's deadline, and its credential's, which `credentialDeadline` (bearer.ts)
 *  computes from the read's time left less a reserve of min(1 s, half the deadline). */
const DISPATCH_READ: CensusEntry = {
  file: "packages/envoy-client/src/dispatch-http.ts",
  kind: "AbortSignal.timeout",
  ms: OPEN_ASKS_TIMEOUT_MS,
}
const CREDENTIAL_WAIT: CensusEntry = {
  file: "packages/envoy-client/src/bearer.ts",
  kind: "AbortSignal.timeout",
  atMost: OPEN_ASKS_TIMEOUT_MS - Math.min(1_000, OPEN_ASKS_TIMEOUT_MS / 2),
  inside: DISPATCH_READ,
}

// The open-asks hook's waits at their bounds, by the census of the starts and timed waits it made:
// each wait ends on the hook's own clock (jj answers only at four times its bound, the token
// command would print only at twice `COMMAND_TIMEOUT_MS`, and in the next row Dispatch takes the
// read and never answers it), and the census must hold exactly those starts and waits, with those
// bounds, whether each ran to its bound or not. So a wait the hook makes past its bound, a start of
// any other program, or one more wait of any length fails the row, and no clock of this machine's,
// loaded or not, enters it. A token command that fails leaves Dispatch off, so no one run meets all
// three waits: each row meets two. The hook's CPU work between its waits is not measured here (the
// start-up allowance of the row above stands for it). A wait the census cannot see, one through no
// timer and no start, is the scan row's.
test("a hook whose jj and token command each run out their time waits on exactly those two, each to its bound", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-waits-command-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const bin = await jjDirectory(
    scratch,
    `sleep ${(4 * JJ_BASE_CHAIN_TIMEOUT_MS) / 1000}; printf '%s\\n' ${JSON.stringify(join(scratch, "jj-user.toml"))}`,
  )
  const requests: string[] = []
  const dispatch = Bun.serve({
    port: 0,
    fetch: (request) => {
      requests.push(new URL(request.url).pathname)
      return Response.json({ error: "no read belongs here" }, { status: 500 })
    },
  })
  await mkdir(join(scratch, ".config", "legion"), { recursive: true })
  await writeFile(
    join(scratch, ".config", "legion", "envoy.json"),
    JSON.stringify({
      dispatch: {
        enabled: true,
        serverUrl: `http://127.0.0.1:${dispatch.port}`,
        token: `!sleep ${(2 * COMMAND_TIMEOUT_MS) / 1000}; printf ${DISPATCH_TOKEN}`,
      },
    }),
  )
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runToItsTimedWaits(
      scratch,
      {
        HOME: scratch,
        TMPDIR: scratch,
        PATH: `${bin}:${process.env["PATH"] ?? ""}`,
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
        CLAUDE_ENV_FILE: envFile,
        LEGION_STATE_DIR: stateDir,
        LEGION_ISSUE_TRAILER: "ACME-7",
      },
      [
        JJ_START,
        JJ_WAIT,
        // The token command's start, in a group of its own under the watch (spawnWatched), and
        // the runner's timer that ends that group at `COMMAND_TIMEOUT_MS` (envoy-config.ts's
        // runCommand): the read waits on the command without holding the process.
        {
          file: "packages/envoy-client/src/process-group.ts",
          kind: "Bun.spawn",
          command: "/bin/sh",
          ms: -1,
        },
        {
          file: "packages/envoy-client/src/envoy-config.ts",
          kind: "setTimeout",
          ms: COMMAND_TIMEOUT_MS,
        },
      ],
      COMMAND_ROW_CEILINGS,
    )

    // It met both waits: it gave up on jj, and its token command ran out its time.
    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).not.toContain("export JJ_CONFIG=")
    expect(result.stdout).toContain(`did not finish within ${COMMAND_TIMEOUT_MS / 1000} s`)
    expect(requests).toEqual([])
  } finally {
    dispatch.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

test("a hook whose jj and Dispatch read each run out their time waits on exactly those two, each to its bound", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-waits-read-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const bin = await jjDirectory(
    scratch,
    `sleep ${(4 * JJ_BASE_CHAIN_TIMEOUT_MS) / 1000}; printf '%s\\n' ${JSON.stringify(join(scratch, "jj-user.toml"))}`,
  )
  // A Dispatch that takes the read and never answers it.
  const reads: string[] = []
  const dispatch = Bun.serve({
    port: 0,
    fetch: (request) => {
      reads.push(request.headers.get("authorization") ?? "")
      return new Promise<Response>(() => {})
    },
  })
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runToItsTimedWaits(
      scratch,
      {
        HOME: scratch,
        TMPDIR: scratch,
        PATH: `${bin}:${process.env["PATH"] ?? ""}`,
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
        CLAUDE_ENV_FILE: envFile,
        LEGION_STATE_DIR: stateDir,
        LEGION_ISSUE_TRAILER: "ACME-7",
        DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
        DISPATCH_TOKEN,
      },
      [JJ_START, JJ_WAIT, DISPATCH_READ, CREDENTIAL_WAIT],
      READ_ROW_CEILINGS,
    )

    // It met both waits: it gave up on jj, and its read reached Dispatch and was never answered.
    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).not.toContain("export JJ_CONFIG=")
    expect(reads.length).toBeGreaterThan(0)
    expect(reads.filter((authorization) => authorization !== `Bearer ${DISPATCH_TOKEN}`)).toEqual(
      [],
    )
    expect(result.stdout).toStartWith("Dispatch authored-ask summary unavailable: ")
  } finally {
    dispatch.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// The success path, end to end: jj answers at once and Dispatch answers the read with one ask, so
// the hook writes its exports and its summary and exits. Its census must hold the same starts and
// waits as the read row, none of them run to its bound, and nothing more: a wait the hook makes
// after the summary is written, or anywhere else on this path, fails here by name.
test("a hook whose jj and Dispatch answer at once writes its summary with no start or wait beyond theirs", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-waits-answered-"))
  const stateDir = join(scratch, "state")
  await mkdir(stateDir)
  const bin = await jjDirectory(
    scratch,
    `printf '%s\\n' ${JSON.stringify(join(scratch, "jj-user.toml"))}`,
  )
  const reads: string[] = []
  const dispatch = Bun.serve({
    port: 0,
    fetch: (request) => {
      reads.push(request.headers.get("authorization") ?? "")
      return Response.json({
        session_id: "s-1",
        as_of: "2026-09-14T00:00:00Z",
        opened_since: false,
        count: 1,
        waiting_on_human: 1,
        waiting_on_agent: 0,
        asks: [
          {
            id: "ask-9",
            ref: "/issues/LEGION-9#ask-9",
            question: "Ship the bundle?",
            kind: "decision",
            urgency: "med",
            created_at: "2026-09-14T00:00:00Z",
            age_seconds: 90,
            priority: 1,
            owner: { issue: { key: "LEGION-9", title: "Bundle" } },
            human_replied: false,
            last_reply: null,
            waiting_on: "human",
          },
        ],
      })
    },
  })
  const envFile = join(scratch, "session-env.sh")
  try {
    const result = await runToItsTimedWaits(
      scratch,
      {
        HOME: scratch,
        TMPDIR: scratch,
        PATH: `${bin}:${process.env["PATH"] ?? ""}`,
        CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
        CLAUDE_ENV_FILE: envFile,
        LEGION_STATE_DIR: stateDir,
        LEGION_ISSUE_TRAILER: "ACME-7",
        DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
        DISPATCH_TOKEN,
      },
      [JJ_START, JJ_WAIT, DISPATCH_READ, CREDENTIAL_WAIT],
      SUCCESS_ROW_CEILINGS,
    )

    // It took both answers: jj's chain is in the env file, and the summary names the ask.
    expect(result.exitCode).toBe(0)
    expect(await readFile(envFile, "utf8")).toContain("export JJ_CONFIG=")
    expect(reads).toEqual([`Bearer ${DISPATCH_TOKEN}`])
    expect(result.stdout).toContain("Ship the bundle?")
  } finally {
    dispatch.stop(true)
    await rm(scratch, { recursive: true, force: true })
  }
})

// The census sees node:child_process's starts only because Bun 1.4.4 starts them through the
// global Bun.spawn and Bun.spawnSync (`hook-timed-waits.ts`). A Bun that stopped would leave the
// census blind to them with every row still passing, so this row starts one of each kind under
// the census and requires it to hold them.
test("the census sees each child a script starts through node:child_process", async () => {
  const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-census-routing-"))
  const script = join(scratch, "starts.ts")
  await writeFile(
    script,
    [
      'import { execFile, execFileSync, spawn, spawnSync } from "node:child_process"',
      'spawnSync("true")',
      'execFileSync("true")',
      'await new Promise((done) => spawn("true").on("exit", done))',
      'await new Promise((done) => execFile("true", () => done(undefined)))',
      "",
    ].join("\n"),
  )
  const record = join(scratch, "census.json")
  try {
    const run = Bun.spawn(["bun", "--preload", timedWaitsPreload, script], {
      env: {
        PATH: process.env["PATH"] ?? "",
        HOME: scratch,
        HOOK_TIMED_WAITS: record,
        ...NO_TRANSPILER_CACHE,
      },
      stdout: "pipe",
      stderr: "pipe",
    })
    const stderr = collected(run.stderr)
    expect(await exitBeforeTheGuard(run, "the routing script", () => stderr.text())).toBe(0)
    const census: Census = JSON.parse(await readFile(record, "utf8"))
    expect(census.waits.map((wait) => `${wait.kind} ${wait.command}`)).toEqual([
      "Bun.spawnSync true",
      "Bun.spawnSync true",
      "Bun.spawn true",
      "Bun.spawn true",
    ])
  } finally {
    await rm(scratch, { recursive: true, force: true })
  }
})

// The hook's other ways to end, each under the census: a machine with no Dispatch configured and no
// Legion pane (no env file, so no exports and no jj), one whose only envoy.json is an old profile's
// file the hook names (an env file, no Legion state, so its exports but no jj), and input the hook
// cannot read, which it throws on before any wait. Each makes no start and no timed wait, so a wait
// added on any of these paths fails its row by name.
const OTHER_EXIT_PATHS: readonly {
  readonly name: string
  readonly setUp: (scratch: string) => Promise<Record<string, string>>
  readonly input?: Record<string, unknown>
  readonly exitCode: number
  readonly stdout: (scratch: string) => string | RegExp
  readonly ceilings: {
    readonly zeroLengthTimers: number
    readonly clockReads: number
    readonly cpuMs: number
  }
}[] = [
  {
    name: "a machine with no Dispatch configured and no Legion pane",
    setUp: async () => ({}),
    exitCode: 0,
    stdout: () => "",
    ceilings: { zeroLengthTimers: 4, clockReads: 0 + 4, cpuMs: HOOK_CPU_CEILING_MS },
  },
  {
    name: "a profile with no envoy.json whose old file the hook names, in a session with an env file",
    setUp: async (scratch) => {
      const configDirectory = join(scratch, "claude-config")
      await mkdir(configDirectory)
      await mkdir(join(scratch, ".config", "opencode"), { recursive: true })
      await writeFile(
        join(scratch, ".config", "opencode", "envoy.json"),
        JSON.stringify({
          dispatch: {
            enabled: true,
            serverUrl: "https://old.dispatch.example",
            token: "old-token",
          },
        }),
      )
      return {
        CLAUDE_CONFIG_DIR: configDirectory,
        CLAUDE_ENV_FILE: join(scratch, "session-env.sh"),
      }
    },
    exitCode: 0,
    stdout: () => /^Dispatch authored-ask summary unavailable: no envoy\.json at /,
    ceilings: { zeroLengthTimers: 4, clockReads: 0 + 4, cpuMs: HOOK_CPU_CEILING_MS },
  },
  {
    name: "input with no session id, which the hook throws on",
    setUp: async () => ({}),
    input: { source: "startup", hook_event_name: "SessionStart" },
    exitCode: 1,
    stdout: () => "",
    ceilings: { zeroLengthTimers: 4, clockReads: 0 + 4, cpuMs: HOOK_CPU_CEILING_MS },
  },
]

for (const path of OTHER_EXIT_PATHS) {
  test(`${path.name}: the hook ends with no start and no timed wait`, async () => {
    const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-waits-path-"))
    try {
      const result = await runToItsTimedWaits(
        scratch,
        {
          HOME: scratch,
          TMPDIR: scratch,
          CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
          ...(await path.setUp(scratch)),
        },
        [],
        path.ceilings,
        [],
        path.input ?? hookInput,
      )
      expect(result.exitCode, result.stderr).toBe(path.exitCode)
      const stdout = path.stdout(scratch)
      if (typeof stdout === "string") expect(result.stdout).toBe(stdout)
      else expect(result.stdout).toMatch(stdout)
    } finally {
      await rm(scratch, { recursive: true, force: true })
    }
  })
}

/** A stand-in that takes every request and never answers it, as a broker, an issuer or a metadata
 *  server that hangs does; a request ends only when its caller's own bound ends it. */
function neverAnswers() {
  return Bun.serve({ port: 0, hostname: "127.0.0.1", fetch: () => new Promise<Response>(() => {}) })
}

/** A preload that makes the hook's fetches of Google's security token service and IAM Credentials,
 *  whose addresses a pool identity takes from no variable, wait unanswered until their signal ends
 *  them, as a service that hangs does; every other fetch is the real one. */
async function silentGooglePreload(directory: string): Promise<string> {
  const preload = join(directory, "silent-google.preload.ts")
  await writeFile(
    preload,
    [
      "const real = globalThis.fetch",
      "const silent = (input: string | URL | Request, init?: RequestInit) => {",
      "  const { hostname } = new URL(input instanceof Request ? input.url : String(input))",
      '  if (hostname !== "sts.googleapis.com" && hostname !== "iamcredentials.googleapis.com") return real(input, init)',
      "  return new Promise<Response>((_, reject) => {",
      "    const signal = init?.signal",
      "    if (signal?.aborted) reject(signal.reason)",
      '    else signal?.addEventListener("abort", () => reject(signal.reason), { once: true })',
      "  })",
      "}",
      "globalThis.fetch = Object.assign(silent, { preconnect: real.preconnect }) as typeof fetch",
      "",
    ].join("\n"),
  )
  return preload
}

/** openid-client's entry as cipher-client resolves it, against the repository's root: the frame its
 *  discovery's deadline is armed in, wherever the store keeps it and whatever its version. */
const OPENID_CLIENT_SOURCE = relative(
  realpathSync(repositoryRoot),
  realpathSync(
    Bun.resolveSync("openid-client", resolve(repositoryRoot, "packages", "cipher-client", "src")),
  ),
)
const TOKEN_CLIENT_SOURCE = "packages/cipher-client/src/token-client.ts"
const DEVICE_LOGIN_SOURCE = "packages/cipher-client/src/device-login.ts"
const DEVICE_LOGIN_LOCK_SOURCE = "packages/cipher-client/src/device-login-lock.ts"

/** The device-login route's waits, the one route the hook's exit leaves to run
 *  (`endOnceWritten`): its trade holds the login's lock and may yet save a rotated refresh token.
 *  So its waits run to their bounds: the trade's attempt, the refresh's two requests
 *  (device-login.ts's BROKER_REQUEST_MS, armed in brokerConfiguration's customFetch), the lock's
 *  heartbeat (device-login-lock.ts:63) and the retry's jittered first wait. */
const DEVICE_LOGIN_ROUTE: readonly CensusEntry[] = [
  DISPATCH_READ,
  CREDENTIAL_WAIT,
  { file: TOKEN_CLIENT_SOURCE, kind: "AbortSignal.timeout", ms: TRADE_ATTEMPT_MS },
  { file: DEVICE_LOGIN_SOURCE, kind: "AbortSignal.timeout", ms: TRADE_ATTEMPT_MS / 2 },
  { file: DEVICE_LOGIN_SOURCE, kind: "AbortSignal.timeout", ms: TRADE_ATTEMPT_MS / 2 },
  { file: DEVICE_LOGIN_LOCK_SOURCE, kind: "setInterval", ms: 5_000 },
  { file: TOKEN_CLIENT_SOURCE, kind: "setTimeout", atMost: RETRY_FIRST_WAIT_MS },
]

/**
 * What the device-login rows check beyond the census: the hook ran its trade to the end and then
 * ended, from its own records rather than a clock. The trade's attempt ran to its bound; every wait
 * that ended after it is the trade's own wind-down (the refresh's request, the lock's heartbeat,
 * cleared as the lock is let go); the one wait left running at the exit is the retry's first, made
 * after the attempt ended; and the hook left no lock beside the login at `login`. These hold under
 * any load, since each is an order of events in the one process. A hook that exits while its trade
 * holds the lock fails here: its attempt never fires, and its lock stays.
 */
async function leftAfterTheTrade(census: Census, login: string): Promise<void> {
  const named = (wait: TimedWait) => `${wait.kind} in ${madeIn(wait)}, ${wait.outcome}`
  const attempt = census.waits.find(
    (wait) =>
      madeIn(wait) === TOKEN_CLIENT_SOURCE &&
      wait.kind === "AbortSignal.timeout" &&
      wait.ms === TRADE_ATTEMPT_MS,
  )
  expect(attempt?.outcome, "the trade's attempt").toBe("fired")
  const gaveUp = attempt?.endedAt ?? Number.POSITIVE_INFINITY
  const windDown = (wait: TimedWait) =>
    (madeIn(wait) === DEVICE_LOGIN_SOURCE && wait.kind === "AbortSignal.timeout") ||
    (madeIn(wait) === DEVICE_LOGIN_LOCK_SOURCE && wait.kind === "setInterval")
  expect(
    census.waits
      .filter((wait) => wait.endedAt !== undefined && wait.endedAt > gaveUp && !windDown(wait))
      .map(named),
    "waits that ended after the trade gave up, beyond its own wind-down",
  ).toEqual([])
  const heartbeat = census.waits.find(
    (wait) => madeIn(wait) === DEVICE_LOGIN_LOCK_SOURCE && wait.kind === "setInterval",
  )
  expect(heartbeat?.outcome, "the lock's heartbeat").toBe("cleared")
  expect((heartbeat?.endedAt ?? 0) >= gaveUp, "the lock let go after the trade gave up").toBe(true)
  expect(
    census.waits
      .filter((wait) => wait.outcome === "armed" || wait.outcome === "repeating")
      .map(
        (wait) =>
          `${named(wait)}, made ${wait.armedAt >= gaveUp ? "after" : "before"} the trade gave up`,
      ),
    "waits still running at the exit",
  ).toEqual([`setTimeout in ${TOKEN_CLIENT_SOURCE}, armed, made after the trade gave up`])
  expect(existsSync(`${login}.lock`), "a lock left beside the login").toBe(false)
}

/** Every route by which the open-asks hook gets a Dispatch credential with no bearer token set:
 *  the secrets broker's trade (`dispatch-config.ts:551-567`) for each machine identity
 *  (`@legion/cipher-client/environment`: kubernetes, metadata, pool, device-login) and for a
 *  person signed in on the machine. The bearer routes are the rows above: the variable (the read
 *  and success rows) and the `!` command (the token-command row); a token file or a written token
 *  is a read of a file, which arms nothing. */
const BROKER_ROUTES: readonly {
  readonly name: string
  readonly env: (scratch: string, silent: string) => Promise<Record<string, string>>
  readonly preloads?: (scratch: string) => Promise<string[]>
  /** The route's CPU ceiling where it is not `HOOK_CPU_CEILING_MS`. */
  readonly cpuMs?: number
  /** What the row checks beyond the census, from the hook's own records and what it left. */
  readonly check?: (census: Census, scratch: string) => Promise<void>
  readonly expected: readonly CensusEntry[]
  /** The route's own clock reads (no zero-length timer on any route), with room for four more. */
  readonly clockReads: number
}[] = [
  {
    name: "a pod's projected token (kubernetes)",
    env: async (scratch, silent) => ({
      AGENT_SECRETS_URL: `http://${silent}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: await projectedTokenFile(scratch),
    }),
    clockReads: 8 + 4,
    expected: [
      DISPATCH_READ,
      CREDENTIAL_WAIT,
      {
        file: "packages/cipher-client/src/token-client.ts",
        kind: "AbortSignal.timeout",
        ms: TRADE_ATTEMPT_MS,
        cut: true,
      },
    ],
  },
  {
    name: "a Compute Engine machine's metadata server (metadata)",
    env: async (_scratch, silent) => ({
      AGENT_SECRETS_URL: `http://${silent}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "metadata",
      AGENT_SECRETS_MACHINE_AUDIENCE: "example-machine-client-id",
      GCE_METADATA_HOST: silent,
    }),
    clockReads: 5 + 4,
    expected: [
      DISPATCH_READ,
      CREDENTIAL_WAIT,
      {
        file: "packages/cipher-client/src/token-client.ts",
        kind: "AbortSignal.timeout",
        ms: TRADE_ATTEMPT_MS,
        cut: true,
      },
      // metadata-identity.ts:21's TIMEOUT_MS, which node:http arms for the request (:79) and its socket.
      {
        file: "packages/cipher-client/src/metadata-identity.ts",
        kind: "setTimeout",
        ms: 10_000,
        cut: true,
      },
      {
        file: "packages/envoy-client/src/broker-credential.ts",
        kind: "setTimeout",
        ms: 10_000,
        cut: true,
      },
    ],
  },
  {
    name: "a workload identity pool (pool)",
    env: async (scratch, silent) => ({
      AGENT_SECRETS_URL: `http://${silent}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "pool",
      AGENT_SECRETS_MACHINE_TOKEN_FILE: await projectedTokenFile(scratch),
      AGENT_SECRETS_MACHINE_AUDIENCE: "example-machine-client-id",
      AGENT_SECRETS_POOL_AUDIENCE:
        "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pods/providers/cluster",
      AGENT_SECRETS_POOL_ACCOUNT: "machine@example.iam.gserviceaccount.com",
    }),
    cpuMs: POOL_CPU_CEILING_MS,
    preloads: async (scratch) => [await silentGooglePreload(scratch)],
    clockReads: 5 + 4,
    expected: [
      DISPATCH_READ,
      CREDENTIAL_WAIT,
      {
        file: "packages/cipher-client/src/token-client.ts",
        kind: "AbortSignal.timeout",
        ms: TRADE_ATTEMPT_MS,
        cut: true,
      },
      // pool-identity.ts:39's POOL_TIMEOUT_MS, at :191.
      {
        file: "packages/cipher-client/src/pool-identity.ts",
        kind: "AbortSignal.timeout",
        ms: 20_000,
        cut: true,
      },
    ],
  },
  {
    name: "a device login (device-login) its variable names",
    env: async (scratch, silent) => ({
      AGENT_SECRETS_URL: `http://${silent}`,
      AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
      AGENT_SECRETS_DEVICE_LOGIN_FILE: await writeDeviceLogin(
        join(scratch, "legion", "device-login.json"),
        `http://${silent}`,
      ),
    }),
    check: (census, scratch) =>
      leftAfterTheTrade(census, join(scratch, "legion", "device-login.json")),
    clockReads: 16 + 4,
    expected: DEVICE_LOGIN_ROUTE,
  },
  // The login where cipher-client keeps it when no variable names one (`deviceLoginFile`): the
  // path `dispatch login --device` writes on a person's machine. The hook must find the lock there
  // too, or it exits holding it and cuts the refresh (Legion's review 6012608384).
  {
    name: "a device login (device-login) at its default path",
    env: async (scratch, silent) => {
      await writeDeviceLogin(
        join(scratch, ".config", "legion", "device-login.json"),
        `http://${silent}`,
      )
      return {
        AGENT_SECRETS_URL: `http://${silent}`,
        AGENT_SECRETS_MACHINE_IDENTITY: "device-login",
      }
    },
    check: (census, scratch) =>
      leftAfterTheTrade(census, join(scratch, ".config", "legion", "device-login.json")),
    clockReads: 16 + 4,
    expected: DEVICE_LOGIN_ROUTE,
  },
  {
    name: "a person signed in on the machine",
    env: async (scratch, silent) => {
      const secret = join(scratch, "client-secret")
      await writeFile(secret, `${fixtureSecret("client")}\n`, { mode: 0o600 })
      await writeSignIn(
        join(scratch, "signins"),
        await signedIn("ada@example.com", {
          issuer: `http://${silent}`,
          client_id: "example-device-client",
        }),
      )
      return {
        AGENT_SECRETS_URL: `http://${silent}`,
        AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
        AGENT_SECRETS_MACHINE_TOKEN_FILE: await projectedTokenFile(scratch),
        AGENT_SECRETS_SIGNIN_ISSUER: `http://${silent}`,
        AGENT_SECRETS_SIGNIN_CLIENT_ID: "example-device-client",
        AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE: secret,
      }
    },
    clockReads: 7 + 4,
    expected: [
      DISPATCH_READ,
      CREDENTIAL_WAIT,
      {
        file: "packages/cipher-client/src/token-client.ts",
        kind: "AbortSignal.timeout",
        ms: TRADE_ATTEMPT_MS,
        cut: true,
      },
      // person-identity.ts:32's ISSUER_TIMEOUT_SECONDS, which openid-client arms for its discovery.
      {
        file: OPENID_CLIENT_SOURCE,
        kind: "AbortSignal.timeout",
        ms: 30_000,
        cut: true,
      },
    ],
  },
]

/** Writes a device login for `broker` at `file`, as `dispatch login --device` leaves one: a fresh
 *  P-256 key as base64 PKCS#8 DER, a credential id, a random refresh token, an expiry 30 days on and
 *  the broker, in a 0600 file in a 0700 directory (cipher-client's `writeDeviceLoginFile` writes
 *  the same). Returns `file`. */
async function writeDeviceLogin(file: string, broker: string): Promise<string> {
  await mkdir(dirname(file), { recursive: true, mode: 0o700 })
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" })
  await writeFile(
    file,
    JSON.stringify({
      key: privateKey.export({ format: "der", type: "pkcs8" }).toString("base64"),
      credential_id: "cred-1",
      refresh_token: fixtureSecret("refresh"),
      expires_at: new Date(Date.now() + 30 * 86_400_000).toISOString(),
      broker,
    }),
    { mode: 0o600 },
  )
  return file
}

/** A pod's projected token file under `scratch`, random as a real token is. */
async function projectedTokenFile(scratch: string): Promise<string> {
  const file = join(scratch, "projected-token")
  await writeFile(file, `${fixtureSecret("proj")}\n`)
  return file
}

// Each broker route at its bound: the broker, the metadata server, the security token service or
// the issuer the route asks first takes the request and never answers it, so the route waits until
// its own bound ends it, and the hook goes on to the read. The census must hold exactly the route's
// starts and waits beside the jj start, its timer and the read's deadline, so a wait added anywhere
// on that route fails here by name.
for (const route of BROKER_ROUTES) {
  test(`a hook whose credential is traded at the broker by ${route.name}, the first service never answering, waits on nothing but that route's bounds`, async () => {
    const scratch = await mkdtemp(join(tmpdir(), "claude-envoy-hook-waits-broker-"))
    const stateDir = join(scratch, "state")
    await mkdir(stateDir)
    const bin = await jjDirectory(
      scratch,
      `printf '%s\\n' ${JSON.stringify(join(scratch, "jj-user.toml"))}`,
    )
    const silent = neverAnswers()
    const dispatch = openAsksServer()
    try {
      const result = await runToItsTimedWaits(
        scratch,
        {
          HOME: scratch,
          TMPDIR: scratch,
          PATH: `${bin}:${process.env["PATH"] ?? ""}`,
          CLAUDE_PLUGIN_DATA: join(scratch, "plugin-data"),
          CLAUDE_ENV_FILE: join(scratch, "session-env.sh"),
          LEGION_STATE_DIR: stateDir,
          LEGION_ISSUE_TRAILER: "ACME-7",
          DISPATCH_URL: `http://127.0.0.1:${dispatch.port}`,
          AGENT_SECRETS_SIGNIN_DIR: join(scratch, "signins"),
          ...(await route.env(scratch, `127.0.0.1:${silent.port}`)),
        },
        [JJ_START, JJ_WAIT, ...route.expected],
        {
          zeroLengthTimers: 4,
          clockReads: route.clockReads,
          cpuMs: route.cpuMs ?? HOOK_CPU_CEILING_MS,
        },
        (await route.preloads?.(scratch)) ?? [],
      )
      expect(result.exitCode).toBe(0)
      expect(result.stdout).toStartWith("Dispatch authored-ask summary")
      await route.check?.(result.census, scratch)
    } finally {
      silent.stop(true)
      dispatch.stop(true)
      await rm(scratch, { recursive: true, force: true })
    }
  })
}

/**
 * The wait sites of the hook's code whose call carries no bound the scan traces, each with what
 * bounds it. An entry names its file, a text of the site's first line, and how many unbounded
 * sites of that file carry that text: one more of them, or one fewer, fails the row. No check
 * reads `bound`: it is the reviewer's note, so a line number in it holds only at the commit that
 * wrote it and moves whenever its file changes above that line, with nothing to say so. The
 * device-login.ts references name functions instead, since that file's lines moved under them
 * more than once.
 */
const REVIEWED_WAIT_SITES: readonly {
  file: string
  text: string
  kind?: string
  count: number
  bound: string
}[] = [
  {
    file: "packages/claude-envoy/hooks/session-hook.ts",
    text: 'Bun.spawn([process.execPath, Bun.main, "answers-refresh"',
    count: 1,
    bound:
      "the answers hook's refresher, started detached and unref'd in the answers mode: no hook " +
      "waits on it",
  },
  {
    file: "packages/claude-envoy/hooks/session-hook.ts",
    text: "Bun.stdin.text",
    count: 1,
    bound:
      "Claude Code's input, which it writes and closes as it starts the hook; nothing of the " +
      "hook's bounds it, and a hook whose input never closes is ended by Claude Code's own timeout",
  },
  {
    file: "packages/pi-shared/src/jj-attribution.ts",
    text: 'Bun.spawn(["jj", "config", "path", "--user"]',
    count: 1,
    bound:
      "raced with userConfigChain's setTimeout(timeoutMs), which kills jj's process group: the " +
      "jj wait the census holds",
  },
  {
    file: "packages/pi-shared/src/jj-attribution.ts",
    text: "new Response(child.",
    count: 2,
    bound: "jj's stdout and stderr, read inside the same race with userConfigChain's setTimeout",
  },
  {
    file: "packages/envoy-client/src/dispatch-http.ts",
    text: 'fetchImpl(input, { ...init, redirect: "manual" })',
    count: 1,
    bound:
      "the fetch behind the client's #fetch, whose every request is made at :2510 or :2853 with a " +
      "signal this scan traces to AbortSignal.timeout; `init` is that request's own",
  },
  {
    file: "packages/envoy-client/src/process-group.ts",
    text: 'Bun.spawn<"pipe", Out, Err>(groupWatchedBy(argv)',
    count: 1,
    bound:
      "spawnWatched's start of a command in a group of its own; the caller that awaits it bounds " +
      "the wait: the `!` token command's runner (envoy-config.ts's runCommand) ends the group at " +
      "setTimeout(`commandTimeoutMs(env)`), COMMAND_TIMEOUT_MS's 10 s unless a test sets " +
      "ENVOY_COMMAND_TIMEOUT_MS, which nothing the hook runs under sets; the census's command row " +
      "holds that timer at exactly 10 s in the hook's own process",
  },
  {
    file: "packages/envoy-client/src/dispatch-cwd.ts",
    text: "execFileAsync(file, args, { cwd, signal })",
    count: 1,
    bound:
      "its signal is tryExec's controller, which a setTimeout(limitMs) aborts: a timer " +
      "the census sees, REMOTE_LOOKUP_MS at its callers",
  },
  {
    file: "packages/envoy-client/src/nats-credential.ts",
    text: "for await (const status of statuses)",
    count: 2,
    bound:
      "two loops over a NATS connection's statuses: the one at :625 runs in a `void` task no " +
      "caller awaits; the one at :711 is awaited (:735) only in a race with `late`, which " +
      "AbortSignal.timeout(reconnectMs) (:707) rejects. The open-asks hook opens no NATS connection",
  },
  {
    file: "packages/cipher-client/src/broker-request.ts",
    text: "proved.fetch(url, requestInit(options, signal, headers))",
    count: 1,
    bound:
      "requestInit merges openid-client's own signal and `signal` into the request's (:112); its " +
      "callers pass AbortSignal.timeout(BROKER_REQUEST_MS) (device-login.ts, brokerConfiguration) and the trade's " +
      "AbortSignal.any([closed, attempt]) (token-client.ts:560), attempt being " +
      "AbortSignal.timeout(this.#attemptMs) (token-client.ts:557)",
  },
  {
    file: "packages/cipher-client/src/device-login.ts",
    text: "fetchImpl(url, requestInit(options, deadline, options.headers))",
    count: 1,
    bound:
      "requestInit (broker-request.ts:112) merges `deadline`, AbortSignal.timeout(BROKER_REQUEST_MS), armed just before it in brokerConfiguration's customFetch",
  },
  {
    file: "packages/cipher-client/src/person-identity.ts",
    text: "send(url, requestInit(init, signal, init.headers))",
    count: 1,
    bound:
      "openid-client calls it with the signal of its own `timeout` (ISSUER_TIMEOUT_SECONDS, :117), " +
      "which requestInit merges with the caller's (broker-request.ts:112)",
  },
  {
    file: "packages/claude-envoy/hooks/session-hook.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "the fetch handed to the open-asks read's DispatchClient (:384), whose every request is made at dispatch-http.ts:2510 or :2853 under a signal this scan traces to AbortSignal.timeout",
  },
  {
    file: "packages/envoy-client/src/dispatch-http.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "DispatchClient's default fetchImpl (:1101), called by #fetch for the requests of :2510 and :2853",
  },
  {
    file: "packages/envoy-client/src/session-answers.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "the answers mode's DispatchClient (:604), whose requests are made at dispatch-http.ts:2510 and :2853 under the client's own deadline; the open-asks mode does not reach it",
  },
  {
    file: "packages/envoy-client/src/dispatch-commands/context.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "the dispatch command's fetchImpl (:331), handed to the same DispatchClient; the open-asks mode does not reach it",
  },
  {
    file: "packages/envoy-client/src/delivery.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "the fetch postDeliveryReply hands its DispatchClient (:167) with the host's userAgent, whose requests are made at dispatch-http.ts:2510 and :2853 under the client's own deadline; the open-asks mode does not reach it",
  },
  {
    file: "packages/envoy-client/src/dispatch-cwd.ts",
    text: "execFile",
    kind: "child_process handed on as a value",
    count: 1,
    bound: "promisified into execFileAsync (:12), whose one call (:28) is reviewed beside it",
  },
  {
    file: "packages/cipher-client/src/token-client.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "the broker client's default fetch (:360), handed to each trade's request (:807) and called at " +
      "broker-request.ts:149 under the trade's signal",
  },
  {
    file: "packages/cipher-client/src/device-login.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 3,
    bound:
      "a device login's default fetch (startDeviceLogin, finishDeviceLogin and refreshDeviceLogin), handed to brokerConfiguration, whose customFetch makes each request under AbortSignal.timeout(BROKER_REQUEST_MS)",
  },
  {
    file: "packages/cipher-client/src/person-identity.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "a person sign-in's default fetch (:118), called only at :128 under openid-client's timeout",
  },
  {
    file: "packages/cipher-client/src/pool-identity.ts",
    text: "fetch",
    kind: "fetch handed on as a value",
    count: 1,
    bound:
      "the pool identity's default fetch (:171), called only at :197 under AbortSignal.timeout(POOL_TIMEOUT_MS)",
  },
  {
    file: "packages/cipher-client/src/metadata-identity.ts",
    text: "request(",
    count: 1,
    bound:
      "node:http's `timeout: TIMEOUT_MS` (:85), on which the call destroys itself (:103): an idle " +
      "bound of the socket, which the census does not see",
  },
]

/** The scan row's limit: the room the claude-envoy job left for one hang in CI run 45437325102 at
 *  06d187e7, its `timeout-minutes: 10` less the 353 s the job took there (247 s), rounded down. It
 *  is an estimate from that one run, not a reading of what the scan costs. The scan runs in a
 *  process of its own (`waitSites`), killed 5 s before this limit, so a scan that load stretches
 *  past it fails this row by name. On the test thread no limit could stop the scan: one run of
 *  this file passed at 315 s, past the 300 s limit of then (builds Job infra-1580-r27ow-46ct9), and
 *  in CI a scan that slow would have run into the job's timeout, which names no row. The scan took
 *  3,431 ms in that CI run, and up to 284,632 ms in a builds Job on two CPUs beside 54 busy loops
 *  (infra-1580-t27-j5-lqlps). */
const SCAN_ROW_LIMIT_MS = 240_000

/** claude-envoy's TypeScript settings, which the scan reads the hook's modules with. */
const packageTsconfig = resolve(import.meta.dir, "..", "tsconfig.json")

// The census sees only waits made through a timer or a process start. So every place the hook's
// own code (every module of this repository its entry reaches, `hook-wait-sites.ts`) can start a
// process, fetch, request, open a socket or read a stream, by whatever name it is called, must
// carry its own bound that the scan traces to a positive `timeout` or to `AbortSignal.timeout`, no
// larger than the hook's budget where it is a number, or be one of the reviewed sites above, each
// with what bounds it. A new site fails here by file and
// line; a reviewed entry whose count of sites changed fails too.
test(
  "every place the open-asks hook's code can wait on a process, a fetch, a socket or a stream carries a bound the census sees, or a reviewed one",
  async () => {
    const modules = reachedModules(hookEntry, repositoryRoot)
    // A bound counts only up to the hook's budget, the timeout hooks.json gives open-asks.
    const config = JSON.parse(
      await readFile(resolve(import.meta.dir, "..", "hooks", "hooks.json"), "utf8"),
    ) as { hooks: Record<string, { hooks: { args?: string[]; timeout?: number }[] }[]> }
    const budgetSeconds = Object.values(config.hooks)
      .flat()
      .flatMap((group) => group.hooks)
      .find((hook) => hook.args?.at(-1) === "open-asks")?.timeout
    expect(budgetSeconds).toBeGreaterThan(0)
    // The scan runs in a process of its own, which is ended at this row's limit as the row is.
    const sites = await waitSites(
      modules,
      repositoryRoot,
      packageTsconfig,
      (budgetSeconds ?? 0) * 1000,
      SCAN_ROW_LIMIT_MS,
    )
    // The scan read the hook's code: the read's fetch and the pool identity's are in it, bounded.
    expect(
      sites
        .filter((site) => site.boundedBy !== undefined)
        .map((site) => `${site.file} ${site.kind}`),
    ).toEqual(
      expect.arrayContaining([
        "packages/envoy-client/src/dispatch-http.ts fetch",
        "packages/cipher-client/src/pool-identity.ts fetch",
      ]),
    )
    const unbounded = sites.filter((site) => site.boundedBy === undefined)
    const reviewedBy = (site: WaitSite) =>
      REVIEWED_WAIT_SITES.find(
        (entry) =>
          entry.file === site.file &&
          site.text.includes(entry.text) &&
          (entry.kind === undefined || entry.kind === site.kind),
      )
    const named = (site: WaitSite) => `${site.file}:${site.line} ${site.kind}: ${site.text}`
    // One verdict names every site by its line: each one no entry reviews, and each site of an
    // entry whose count changed, so a new site beside a reviewed one is named too.
    expect(
      {
        unreviewed: unbounded.filter((site) => reviewedBy(site) === undefined).map(named),
        miscounted: REVIEWED_WAIT_SITES.flatMap((entry) => {
          const held = unbounded.filter((site) => reviewedBy(site) === entry)
          return held.length === entry.count
            ? []
            : [
                `${entry.file}: ${entry.text}: ${held.length} sites, not ${entry.count}: ${held.map(named).join("; ")}`,
              ]
        }),
      },
      `wait sites with no bound the census sees, of ${sites.length} in ${modules.length} modules`,
    ).toEqual({ unreviewed: [], miscounted: [] })
  },
  SCAN_ROW_LIMIT_MS,
)
