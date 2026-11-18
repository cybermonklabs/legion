import { afterAll, afterEach, beforeAll, expect, test } from "bun:test"
import { mkdirSync, mkdtempSync, rmSync } from "node:fs"
import { join } from "node:path"
import { restoreEnvironment } from "@legion/test-fixtures/environment"
import { resolveModSocketPath } from "../hooks/mod-protocol"
import { modSocketScratch, sessionModSocket } from "./mod-socket-scratch"

// modSocketScratch (tests/mod-socket-scratch.ts) under the TMPDIR lengths a machine has: CI's /tmp,
// a dev box's 45-byte TMPDIR, and a 119-byte one. Every TMPDIR here is a directory of this file's own
// under a short root in /tmp, padded to its length, so the rows read the same whatever TMPDIR the
// run was given. The prefix is envoy-channel-mcp.test.ts's, the longest a caller passes, whose
// sessions found no mod socket under the dev box's TMPDIR before the helper fell back.
const PREFIX = "claude-envoy-mcp-"
const SESSION = "scratch"

restoreEnvironment(["TMPDIR"])

/** The short root every TMPDIR of this file is made in, and the directories the helper made. */
let root = ""
const made: string[] = []
beforeAll(() => {
  root = mkdtempSync("/tmp/mss-")
})
afterEach(() => {
  for (const dir of made.splice(0)) rmSync(dir, { force: true, recursive: true })
})
afterAll(() => {
  rmSync(root, { force: true, recursive: true })
})

/** A fresh directory under `root` whose path is exactly `bytes` bytes, made TMPDIR. */
function tmpdirOf(bytes: number): string {
  const dir = join(root, "t".repeat(bytes - root.length - 1))
  mkdirSync(dir)
  process.env["TMPDIR"] = dir
  return dir
}

/** The scratch directory the helper makes, and what it printed on stderr while making it. */
async function scratch(): Promise<{ readonly dir: string; readonly notes: string[] }> {
  const notes: string[] = []
  const printed = console.error
  console.error = (...parts: unknown[]) => {
    notes.push(parts.map((part) => String(part)).join(" "))
  }
  try {
    const dir = await modSocketScratch(PREFIX)
    made.push(dir)
    return { dir, notes }
  } finally {
    console.error = printed
  }
}

for (const bytes of [45, 119]) {
  test(`under a ${bytes}-byte TMPDIR the directory is under /tmp, its socket path is one the product takes, and the fallback is said`, async () => {
    const tmp = tmpdirOf(bytes)
    expect(Buffer.byteLength(tmp)).toBe(bytes)
    // The control: a directory made directly under that TMPDIR, as the helper made it before it
    // fell back, names no socket, so this row fails if the helper stops falling back.
    const direct = mkdtempSync(join(tmp, PREFIX))
    expect(resolveModSocketPath(SESSION, { XDG_RUNTIME_DIR: direct }).refusal).toContain(
      "every private base makes a socket path longer than 100 bytes",
    )

    const { dir, notes } = await scratch()
    expect(dir.startsWith(`/tmp/${PREFIX}`)).toBe(true)
    const resolution = resolveModSocketPath(SESSION, { XDG_RUNTIME_DIR: dir })
    expect(resolution.refusal).toBeUndefined()
    expect(resolution.path?.startsWith(`${dir}/`)).toBe(true)
    expect(notes).toHaveLength(1)
    expect(notes[0]).toStartWith(
      `modSocketScratch: every private base makes a socket path longer than 100 bytes (${tmp}/`,
    )
    expect(notes[0]).toEndWith("; falling back to /tmp")
  })
}

test("under a TMPDIR the socket fits under, the directory is under that TMPDIR and nothing is said", async () => {
  const tmp = tmpdirOf(root.length + 2)
  const { dir, notes } = await scratch()
  expect(dir.startsWith(`${tmp}/${PREFIX}`)).toBe(true)
  expect(resolveModSocketPath(SESSION, { XDG_RUNTIME_DIR: dir }).refusal).toBeUndefined()
  expect(notes).toEqual([])
})

// The tests that read a session's socket (envoy-channel-stall, envoy-channel-mcp, mod-link) throw
// through sessionModSocket when there is none. Its error carries the reason resolveModSocketPath
// gives, so a base past the 100-byte limit reads as that and not as a bare "names no mod socket".
test("a session whose socket path passes the limit fails with the reason", () => {
  const tooDeep = join(root, "d".repeat(100 - root.length))
  expect(() => sessionModSocket(SESSION, { XDG_RUNTIME_DIR: tooDeep })).toThrow(
    `the session names no mod socket: every private base makes a socket path longer than 100 bytes (${tooDeep}/`,
  )
  expect(() => sessionModSocket(SESSION, {})).toThrow(
    "the session names no mod socket: none of XDG_RUNTIME_DIR",
  )
  // A base it fits under answers the path the resolver gives, which the server binds.
  const fits = join(root, "f")
  const resolved = resolveModSocketPath(SESSION, { XDG_RUNTIME_DIR: fits }).path
  expect(resolved?.startsWith(`${fits}/claude-envoy/ce-`)).toBe(true)
  expect(sessionModSocket(SESSION, { XDG_RUNTIME_DIR: fits })).toBe(resolved ?? "no path")
})
