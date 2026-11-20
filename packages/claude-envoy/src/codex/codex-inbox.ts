import { randomBytes } from "node:crypto"
import { mkdir, readdir, readFile, rename, rm, rmdir, writeFile } from "node:fs/promises"
import { join } from "node:path"
import { processExists } from "../process-exists"

/**
 * The Codex plugin's inbox: messages waiting for the running turn of one session.
 *
 * Codex gives a plugin no way to write into a running conversation, but a `PostToolUse` hook's
 * `additionalContext` reaches the model in its very next request. So a message that arrives while
 * a turn runs is written here as one file, and two parties may take it: the `PostToolUse` hook,
 * which puts it into the turn at the next tool call, and the MCP server, which hands it to
 * `codex queue` when the turn ends with it still here. Each takes a message by renaming its file
 * into a directory of its own, so exactly one of them delivers it: `rename` within one directory
 * tree is atomic, and the second rename of the same file fails.
 *
 * Layout under the session's directory: `waiting/<order>-<random>.json` is a message nobody took,
 * `taken/` holds what the hook handed Codex and `queued/` what the server handed to `codex queue`.
 * `claimed/<pid>/` holds what the hook process `pid` took and is still writing to Codex: the hook
 * moves each message there on into `taken/` only once Codex has every byte of its output, and back
 * into `waiting/` when the write failed, so output that never reached Codex loses no message. What
 * a hook that is gone held (Codex kills a hook at its timeout, and with the turn it interrupts) goes
 * back into `waiting/` at the next hook or sweep (`releaseClaims`).
 */
export interface CodexInboxItem {
  /** The text the model is to read. */
  readonly content: string
  /** The Envoy event this message is, where it is one. */
  readonly eventId?: string
  /** The Dispatch message this is, when it is a person's own message. */
  readonly dispatchMessageId?: string
}

export type CodexInboxTaker = "hook" | "server"

const TAKEN_DIRECTORY: Record<CodexInboxTaker, string> = { hook: "taken", server: "queued" }
const CLAIMED_DIRECTORY = "claimed"

/** The inbox directory of one session, under the plugin's state directory. */
export function codexInboxDirectory(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, "codex-inbox", encodeURIComponent(sessionId))
}

/** A file name for a new line: it sorts after every name chosen before it in this process. */
let lastOrder = 0
export function inboxItemName(): string {
  const order = Math.max(Date.now(), lastOrder + 1)
  lastOrder = order
  return `${String(order).padStart(15, "0")}-${randomBytes(4).toString("hex")}.json`
}

/** Writes `item` as a waiting message named `name` and answers the name, which `takenBy` and
 *  `withdrawWaiting` look up. The file is renamed into place whole, so a reader never sees half of
 *  it. A caller that must be ready for the line before anyone can see it chooses `name` first. */
export async function putInbox(
  directory: string,
  item: CodexInboxItem,
  name: string = inboxItemName(),
): Promise<string> {
  const waiting = join(directory, "waiting")
  await mkdir(waiting, { recursive: true, mode: 0o700 })
  const temporary = join(directory, `.${name}.tmp`)
  await writeFile(temporary, JSON.stringify(item), { mode: 0o600 })
  await rename(temporary, join(waiting, name))
  return name
}

/** Who took the message named `name`, or undefined while it waits (or when nobody knows it). */
export async function takenBy(
  directory: string,
  name: string,
): Promise<CodexInboxTaker | undefined> {
  for (const taker of ["hook", "server"] as const) {
    try {
      await readFile(join(directory, TAKEN_DIRECTORY[taker], name))
      return taker
    } catch {
      // Not taken by this one.
    }
  }
  return undefined
}

/**
 * Takes the one line `name` out of `directory`'s waiting lines, where nothing took it yet: answers
 * whether it was still waiting, so a caller refuses its write only when no taker can deliver it.
 */
export async function withdrawWaiting(directory: string, name: string): Promise<boolean> {
  try {
    await rm(join(directory, "waiting", name))
    return true
  } catch {
    return false
  }
}

/**
 * Removes the messages waiting in `directory` that no write of this process waits for: lines an
 * earlier server of the session left when it stopped. Each one's write never resolved, so the
 * Envoy frame it carried was never answered and Envoy sends it again to whoever binds the session
 * next; delivering the file as well would give the session the message twice (after `codex
 * resume`, for one). `keep` names the files to leave.
 */
export async function discardWaiting(
  directory: string,
  keep: (name: string) => boolean = () => false,
): Promise<number> {
  const waiting = join(directory, "waiting")
  let names: string[]
  try {
    names = await readdir(waiting)
  } catch {
    return 0
  }
  let removed = 0
  for (const name of names) {
    if (!name.endsWith(".json") || keep(name)) continue
    try {
      await rm(join(waiting, name))
      removed++
    } catch {
      // Taken by the hook or the sweep meanwhile.
    }
  }
  return removed
}

/**
 * The most a `PostToolUse` hook hands Codex at once, in bytes of `inboxContext`'s text. Codex
 * caps a hook's `additionalContext` at 2,500 tokens by default (`hooks/src/output_spill.rs` in
 * 0.154.0); past it the model gets a preview and the rest goes to a file, while the message would
 * count as delivered whole. A token of a tokenizer like the one Codex's models use rarely covers
 * fewer than about three and a half bytes of English, so 8,000 bytes leaves room under the cap for
 * ordinary text. A message dense in rare characters can still run past it.
 */
export const HOOK_CONTEXT_MAX_BYTES = 8_000

/**
 * Takes waiting messages for the server, oldest first, and answers them: they are its to hand to
 * `codex queue`. A message another taker renamed first is skipped, as is one whose file cannot be
 * read once taken (it is left in `queued/`, where nothing reads it again).
 */
export async function takeInbox(
  directory: string,
): Promise<{ readonly name: string; readonly item: CodexInboxItem }[]> {
  return moveWaiting(directory, join(directory, TAKEN_DIRECTORY.server))
}

/** What a `PostToolUse` hook took: its messages, and the step that moves them on. */
export interface CodexInboxClaim {
  readonly taken: readonly { readonly name: string; readonly item: CodexInboxItem }[]
  /**
   * Moves every message of the claim on: into `taken/`, where the server finds it delivered, when
   * `delivered` says Codex has every byte of the hook's output; back into `waiting/` when it does
   * not. A file the claim could not read goes into `taken/` either way, where nothing reads it
   * again.
   */
  settle(delivered: boolean): Promise<void>
}

/**
 * Takes waiting messages for this process, a `PostToolUse` hook, into `claimed/<pid>/`, oldest
 * first, after putting back what a hook that is gone held (`releaseClaims`). A message another
 * taker renamed first is skipped. They stay there, delivered to nobody, until the claim is settled.
 *
 * With `maxContextBytes`, it stops at the first message that would take `inboxContext`'s text past
 * that many bytes, and leaves it and every later one waiting, in order, for `codex queue` at the
 * turn's end, which takes a message of any length.
 */
export async function claimInbox(
  directory: string,
  maxContextBytes?: number,
): Promise<CodexInboxClaim> {
  await releaseClaims(directory)
  const held = join(directory, CLAIMED_DIRECTORY, String(process.pid))
  const taken = await moveWaiting(directory, held, maxContextBytes)
  return {
    taken,
    async settle(delivered) {
      if (taken.length === 0) return
      const to = join(directory, delivered ? TAKEN_DIRECTORY.hook : "waiting")
      await mkdir(to, { recursive: true, mode: 0o700 })
      for (const { name } of taken) {
        try {
          await rename(join(held, name), join(to, name))
        } catch {
          // Put back meanwhile by a sweep that found this process gone: it waits again.
        }
      }
      await moveAll(held, join(directory, TAKEN_DIRECTORY.hook))
    },
  }
}

/**
 * Puts back into `waiting/` every message a hook process that no longer runs held in
 * `claimed/<pid>/`: Codex killed it at its timeout, or with the turn it interrupted, before it
 * settled its claim, so Codex has none of its output. What a hook that still runs holds stays.
 */
export async function releaseClaims(directory: string): Promise<void> {
  const claimed = join(directory, CLAIMED_DIRECTORY)
  let holders: string[]
  try {
    holders = await readdir(claimed)
  } catch {
    return
  }
  for (const holder of holders) {
    if (!/^\d+$/.test(holder) || processExists(Number(holder))) continue
    await moveAll(join(claimed, holder), join(directory, "waiting"))
  }
}

/** Moves every file of `from` into `to`, then removes `from`. */
async function moveAll(from: string, to: string): Promise<void> {
  let names: string[]
  try {
    names = await readdir(from)
  } catch {
    return
  }
  if (names.length > 0) await mkdir(to, { recursive: true, mode: 0o700 })
  for (const name of names) {
    try {
      await rename(join(from, name), join(to, name))
    } catch {
      // Moved meanwhile by the claim's own settle, or by another sweep.
    }
  }
  await rmdir(from).catch(() => undefined)
}

/**
 * Moves waiting messages into `destination`, oldest first, and answers them. A message another
 * taker renamed first is skipped, as is one whose file cannot be read once moved (it is left in
 * `destination`). With `maxContextBytes`, it stops at the first message that would take
 * `inboxContext`'s text past that many bytes.
 */
async function moveWaiting(
  directory: string,
  destination: string,
  maxContextBytes?: number,
): Promise<{ readonly name: string; readonly item: CodexInboxItem }[]> {
  const waiting = join(directory, "waiting")
  let names: string[]
  try {
    names = (await readdir(waiting)).filter((name) => name.endsWith(".json")).sort()
  } catch {
    return []
  }
  if (names.length === 0) return []
  await mkdir(destination, { recursive: true, mode: 0o700 })
  const taken: { name: string; item: CodexInboxItem }[] = []
  for (const name of names) {
    if (maxContextBytes !== undefined) {
      let waitingItem: unknown
      try {
        waitingItem = JSON.parse(await readFile(join(waiting, name), "utf8"))
      } catch {
        continue
      }
      if (typeof (waitingItem as CodexInboxItem | null)?.content !== "string") continue
      const items = [...taken.map(({ item }) => item), waitingItem as CodexInboxItem]
      if (Buffer.byteLength(inboxContext(items)) > maxContextBytes) break
    }
    try {
      await rename(join(waiting, name), join(destination, name))
    } catch {
      continue
    }
    try {
      const parsed: unknown = JSON.parse(await readFile(join(destination, name), "utf8"))
      if (
        typeof parsed === "object" &&
        parsed !== null &&
        typeof (parsed as CodexInboxItem).content === "string"
      ) {
        taken.push({ item: parsed as CodexInboxItem, name })
      }
    } catch {
      // Unreadable once taken: nothing else will try it.
    }
  }
  return taken
}

/** The text a `PostToolUse` hook hands Codex for messages it took: each message whole, in order,
 *  under one line saying where they came from. A person's own message says it is theirs, since
 *  Codex frames hook text as an instruction rather than as something a person typed. */
export function inboxContext(items: readonly CodexInboxItem[]): string {
  return [
    "Messages delivered to this session while it worked (Envoy, through the Envoy plugin):",
    ...items.map((item) =>
      item.dispatchMessageId === undefined
        ? item.content
        : `A person wrote to you from Dispatch (message ${item.dispatchMessageId}):\n${item.content}`,
    ),
  ].join("\n\n")
}
