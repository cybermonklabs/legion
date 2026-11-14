import { randomBytes } from "node:crypto"
import { mkdir, readdir, readFile, rename, writeFile } from "node:fs/promises"
import { join } from "node:path"

/**
 * The Codex plugin's inbox: messages waiting for the running turn of one session (INFRA-3558).
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
 * `taken/` holds what the hook took and `queued/` what the server handed to `codex queue`.
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

/** The inbox directory of one session, under the plugin's state directory. */
export function codexInboxDirectory(stateDirectory: string, sessionId: string): string {
  return join(stateDirectory, "codex-inbox", encodeURIComponent(sessionId))
}

/** A file name that sorts after every name written before it in this process. */
let lastOrder = 0
function itemName(): string {
  const order = Math.max(Date.now(), lastOrder + 1)
  lastOrder = order
  return `${String(order).padStart(15, "0")}-${randomBytes(4).toString("hex")}.json`
}

/** Writes `item` as a waiting message and answers its file name, which `isWaiting` and
 *  `takenBy` look up. The file is renamed into place whole, so a reader never sees half of it. */
export async function putInbox(directory: string, item: CodexInboxItem): Promise<string> {
  const waiting = join(directory, "waiting")
  await mkdir(waiting, { recursive: true, mode: 0o700 })
  const name = itemName()
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
 * Takes every waiting message for `taker`, oldest first, and answers them. A message another
 * taker renamed first is skipped, as is one whose file cannot be read once taken (it is left in
 * the taker's directory, where nothing reads it again).
 */
export async function takeInbox(
  directory: string,
  taker: CodexInboxTaker,
): Promise<{ readonly name: string; readonly item: CodexInboxItem }[]> {
  const waiting = join(directory, "waiting")
  let names: string[]
  try {
    names = (await readdir(waiting)).filter((name) => name.endsWith(".json")).sort()
  } catch {
    return []
  }
  if (names.length === 0) return []
  const destination = join(directory, TAKEN_DIRECTORY[taker])
  await mkdir(destination, { recursive: true, mode: 0o700 })
  const taken: { name: string; item: CodexInboxItem }[] = []
  for (const name of names) {
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
