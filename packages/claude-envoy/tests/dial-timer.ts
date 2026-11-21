// The record of the NATS dial timers a channel server process arms, for the rows that stop a
// server while its dial waits on a hub that does not answer (channel-fatal-exit.test.ts's mute hub,
// envoy-channel-start-reopen.test.ts's held replacement dial). What decides such a row is how the
// process ended, read from the process itself, not when: a stop that leaves the dial's wait alone
// exits 0 too, but only once nats.js's own dial timer has fired.
import { appendFileSync } from "node:fs"
import { mkdir, readFile, writeFile } from "node:fs/promises"
import { createRequire } from "node:module"
import { join } from "node:path"
import { natsConnectionOptions } from "@legion/envoy-client/nats-connection"
import { completeLines } from "@legion/test-fixtures/transcript-lines"

const require = createRequire(import.meta.url)
const NATS_PROTOCOL = require.resolve("nats/lib/nats-base-client/protocol.js")

/** One line of the record: a dial timer armed, cleared or fired, numbered in the order it was
 *  armed; the process's `exit`; or a mark the row wrote itself (`markDialTimers`). */
export type DialTimerLine =
  | { readonly armed: number }
  | { readonly cleared: number }
  | { readonly fired: number }
  | { readonly exit: number }
  | { readonly row: string }

/** The timer nats.js's protocol arms for a dial a channel server starts. The installed protocol
 *  takes `this.options.timeout || <number>`, and the server's connection options set the timeout
 *  (`natsConnectionOptions`): the number is read from there, else from that very source, never
 *  from every timer that happens to have a large delay. */
async function natsDialTimeoutMs(): Promise<number> {
  const options = natsConnectionOptions({
    servers: [],
    name: "dial-timer-record",
    credential: { kind: "none" },
  })
  if (options.timeout !== undefined) return options.timeout
  const source = await readFile(NATS_PROTOCOL, "utf8")
  const defaultTimeout = /this\.options\.timeout \|\| (\d+)/.exec(source)?.[1]
  if (defaultTimeout === undefined)
    throw new Error(`could not read nats's dial timeout in ${NATS_PROTOCOL}`)
  return Number(defaultTimeout)
}

/**
 * A module for a server's `bun --preload`, made in `<home>/dial-timer`, and the record it appends
 * to, one JSON line a step: each timer with the exact timeout the server's connection options
 * select whose arming call comes from nats's protocol source, as it is armed, cleared and fired,
 * and the process's exit with its code. A row's own marks go into the same file, so one order
 * holds both.
 */
export async function dialTimerPreload(
  home: string,
): Promise<{ readonly preload: string; readonly record: string }> {
  const directory = join(home, "dial-timer")
  const preload = join(directory, "preload.js")
  const record = join(directory, "record.jsonl")
  await mkdir(directory, { recursive: true })
  await writeFile(
    preload,
    [
      'import { appendFileSync } from "node:fs"',
      `const timeout = ${await natsDialTimeoutMs()}`,
      `const protocol = ${JSON.stringify(NATS_PROTOCOL)}`,
      `const record = ${JSON.stringify(record)}`,
      'const put = (line) => appendFileSync(record, JSON.stringify(line) + "\\n")',
      "const set = globalThis.setTimeout",
      "const clear = globalThis.clearTimeout",
      "const dials = new Map()",
      "let armed = 0",
      "globalThis.setTimeout = (callback, delay, ...args) => {",
      '  const dial = Number(delay) === timeout && (new Error().stack ?? "").includes(protocol)',
      "  const number = dial ? ++armed : undefined",
      "  const handle = set((...callbackArgs) => {",
      "    if (number !== undefined && dials.delete(handle)) put({ fired: number })",
      "    return callback(...callbackArgs)",
      "  }, delay, ...args)",
      "  if (number !== undefined) {",
      "    dials.set(handle, number)",
      "    put({ armed: number })",
      "  }",
      "  return handle",
      "}",
      "globalThis.clearTimeout = (handle) => {",
      "  const number = dials.get(handle)",
      "  if (number !== undefined && dials.delete(handle)) put({ cleared: number })",
      "  return clear(handle)",
      "}",
      'process.on("exit", (code) => put({ exit: code }))',
      "",
    ].join("\n"),
  )
  return { preload, record }
}

/** Writes the row's mark `row` into the record, after every step the server recorded so far. */
export function markDialTimers(record: string, row: string): void {
  appendFileSync(record, `${JSON.stringify({ row })}\n`)
}

/** The record as the server and the row left it: nothing where the server armed nothing and the
 *  row marked nothing. */
export async function readDialTimers(record: string): Promise<readonly DialTimerLine[]> {
  const text = await readFile(record, "utf8").catch(() => "")
  return completeLines(text).map((line) => JSON.parse(line) as DialTimerLine)
}

/**
 * How the dial the process armed last stood as it exited: `armed and not fired` where its timer
 * was still waiting, `fired` where the process lived until it ran out, and `none armed` where it
 * was cleared (the dial ended) or no dial was armed; `no exit` where the process never ran its
 * exit event (a signal killed it).
 */
export function lastDialAtExit(lines: readonly DialTimerLine[]): string {
  if (!lines.some((line) => "exit" in line)) return "no exit"
  const last = lines.findLast((line) => "armed" in line)
  if (last === undefined || !("armed" in last)) return "none armed"
  const { armed } = last
  if (lines.some((line) => "fired" in line && line.fired === armed)) return "fired"
  if (lines.some((line) => "cleared" in line && line.cleared === armed)) return "none armed"
  return "armed and not fired"
}

/** The dial timers that fired after the row's mark `row`: each a dial the process waited out. */
export function firedAfter(lines: readonly DialTimerLine[], row: string): readonly number[] {
  const marked = lines.findIndex((line) => "row" in line && line.row === row)
  if (marked === -1) throw new Error(`the dial-timer record holds no mark "${row}"`)
  return lines.slice(marked + 1).flatMap((line) => ("fired" in line ? [line.fired] : []))
}
