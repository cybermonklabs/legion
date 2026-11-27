import { beforeEach, expect, setDefaultTimeout, test } from "bun:test"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import { join, resolve } from "node:path"
import { consumerDelivery } from "@legion/envoy-client/test/subscribe-answer"
import { FakeNatsServer } from "./fake-nats-server"
import { beforeTheGuard, collected, ROW_GUARD_MS, rowStarted } from "./row-deadline"
import { NO_TRANSPILER_CACHE } from "./transpiler-cache"

const entry = resolve(import.meta.dir, "..", "bin", "envoy-channel.ts")

// The row waits for the server's registration and then for its exit, and decides on what the
// listener was asked; a server that ends before it registers is named with its exit, and a
// registration or an exit that never comes fails the row by name before
// the hang guard this file sets as its rows' default (`ROW_GUARD_MS`, 120 s, `./row-deadline.ts`). A fixed 20 s decided it before: at a load near 1,000
// the server, writing Bun's transpiler cache into its empty HOME first (`./transpiler-cache.ts`),
// had not registered in 20 s.
beforeEach(rowStarted)
setDefaultTimeout(ROW_GUARD_MS)

test("a hangup from the Claude process group deregisters the session before the server exits", async () => {
  // tmux kill-session (and a closing terminal) hang up the whole process group;
  // the channel server shares it with `claude`, so SIGHUP arrives directly and
  // must still lead to `DELETE /v1/sessions/<id>` rather than a silent death
  // that leaves the session live in the registry until its TTL.
  const broker = new FakeNatsServer()
  const calls: string[] = []
  const registered = Promise.withResolvers<void>()
  const listener = Bun.serve({
    port: 0,
    fetch: async (request) => {
      const url = new URL(request.url)
      calls.push(`${request.method} ${url.pathname}`)
      if (request.method === "POST" && url.pathname === "/v1/interests/subscribe") {
        const body = (await request.json()) as { session_id: string; topics: string[] }
        registered.resolve()
        return Response.json({
          session_id: body.session_id,
          machine_id: "test",
          dir: "/tmp",
          topics: body.topics,
          // Every stand-in listener answers the session's own consumer
          // (`@legion/envoy-client/test/subscribe-answer`): one that answers no delivery leaves
          // its session refused inbound, which is a state of its own and not this case's.
          delivery: consumerDelivery(body.session_id),
        })
      }
      if (request.method === "DELETE") return new Response(null, { status: 200 })
      return Response.json({ error: "not found" }, { status: 404 })
    },
  })
  const stateDirectory = await mkdtemp(join(tmpdir(), "claude-envoy-lifecycle-"))
  const server = Bun.spawn([process.execPath, entry], {
    cwd: stateDirectory,
    env: {
      PATH: process.env["PATH"] ?? "",
      HOME: stateDirectory,
      ENVOY_NATS_URL: broker.url,
      ENVOY_URL: `http://127.0.0.1:${listener.port}`,
      CLAUDE_CODE_SESSION_ID: "ses_hangup",
      CLAUDE_PLUGIN_DATA: stateDirectory,
      ...NO_TRANSPILER_CACHE,
    },
    stdin: "pipe",
    stdout: "pipe",
    stderr: "pipe",
  })
  const stderr = collected(server.stderr)
  const state = () => `the listener was asked [${calls.join(", ")}]; stderr: ${stderr.text()}`

  try {
    await beforeTheGuard(
      Promise.race([
        registered.promise,
        server.exited.then(async (code) => {
          throw new Error(
            `the channel server exited ${code} before it registered: ${await stderr.ended}`,
          )
        }),
      ]),
      "the channel server's registration",
      state,
    )
    server.kill("SIGHUP")
    const exitCode = await beforeTheGuard(
      server.exited,
      "the channel server's exit on SIGHUP",
      state,
    )

    expect(exitCode).toBe(0)
    expect(calls).toContain("DELETE /v1/sessions/ses_hangup")
  } finally {
    server.kill("SIGKILL")
    listener.stop(true)
    await broker.stop()
    await rm(stateDirectory, { recursive: true, force: true })
  }
})
