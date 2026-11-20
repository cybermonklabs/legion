import { isCleanUrl } from "@legion/contracts/log-text"
import { z } from "zod"

const SendArgumentsSchema = z.tuple([z.string().min(1), z.string().min(1)]).rest(z.string().min(1))

/** The flag that sends a message under a key the caller names: the one an unconfirmed send
 *  printed, so the listener drops the resend as a repeat if the first copy arrived. */
export const IDEMPOTENCY_KEY_FLAG = "--idempotency-key"

export type SendArguments = {
  readonly targetSession: string
  readonly message: string
  /** The key the send goes under, where the caller names one; one is minted otherwise. */
  readonly idempotencyKey?: string
}

/**
 * `[--idempotency-key <key>] <target session> <message words…>`. The flag comes first, so a
 * message whose words begin with it is still a message.
 */
export function parseSendArguments(arguments_: readonly string[]): SendArguments {
  let rest = arguments_
  let idempotencyKey: string | undefined
  if (rest[0] === IDEMPOTENCY_KEY_FLAG) {
    idempotencyKey = z.string().min(1).parse(rest[1])
    rest = rest.slice(2)
  }
  const [targetSession, ...words] = SendArgumentsSchema.parse(rest)
  return {
    targetSession,
    message: words.join(" "),
    ...(idempotencyKey === undefined ? {} : { idempotencyKey }),
  }
}

/**
 * `value` as one word of a POSIX shell command that bash and zsh both read as the same text:
 * bare where every character is one neither shell expands, single-quoted otherwise, with each `'`
 * closed, escaped and reopened. A leading `=` is quoted too: zsh's EQUALS option, on by default,
 * reads a bare `=word` as the path of the command `word`.
 */
export function shellWord(value: string): string {
  return /^[A-Za-z0-9_./:@%+-][A-Za-z0-9_./:=@%+-]*$/.test(value)
    ? value
    : `'${value.replaceAll("'", "'\\''")}'`
}

/** What the resend command an unconfirmed send prints is built of. */
export interface Resend {
  /** The command's own script (`bun <script>`). */
  readonly script: string
  /** The directory the send ran in: a checkout's own `.envoy/envoy.json` is read from it. */
  readonly cwd: string
  /**
   * The settings, by name and value, that chose the send's listener and credential (the envoy.json
   * it read, `ENVOY_URL`, `ENVOY_TOKEN_FILE`, the session): each is carried as `NAME=value` ahead
   * of the command, so the resend goes where the send went from any shell it is run in.
   */
  readonly settings: readonly (readonly [name: string, value: string])[]
  readonly idempotencyKey: string
  readonly targetSession: string
  readonly message: string
}

/**
 * What an unconfirmed send prints of its resend: the command, or, where a setting it carries is a
 * URL no printed line may name, that setting's name and no command.
 */
export type ResendCommand = { readonly command: string } | { readonly unprinted: string }

/** Whether `name` names a URL setting (`ENVOY_URL`), which prints only as `isCleanUrl` allows. */
const URL_SETTING = /_URL$/

/**
 * The command that sends the same message again under its key, to the same listener, as
 * `envoy-send` was run: `cd <cwd> && NAME=value … bun <script> --idempotency-key <key> <target>
 * <message>`. The listener drops it as a repeat if the first copy arrived.
 *
 * A URL setting is printed only where it is a plain http(s) URL with no place a credential can sit
 * (`isCleanUrl`, `@legion/contracts/log-text`), the one rule every client names a configured URL
 * by. Where one is not, such as a URL with user info of any length, no command is built, only the
 * setting's name (`unprinted`): the scrub of held secrets takes out a held value of 10 characters
 * or more, so a shorter user and password would print whole, and the URL with its user info taken
 * out could still print part of it, since the parser can split such a URL otherwise than its
 * writer meant (`https://user:p@ssw0rd` reads `ssw0rd` as the host).
 */
export function resendCommand(resend: Resend): ResendCommand {
  const unclean = resend.settings.find(
    ([name, value]) => URL_SETTING.test(name) && !isCleanUrl(value),
  )
  if (unclean !== undefined) return { unprinted: unclean[0] }
  const assignments = resend.settings.map(([name, value]) => `${name}=${shellWord(value)}`)
  const command = [
    "bun",
    resend.script,
    IDEMPOTENCY_KEY_FLAG,
    resend.idempotencyKey,
    resend.targetSession,
    resend.message,
  ].map(shellWord)
  return { command: `cd ${shellWord(resend.cwd)} && ${[...assignments, ...command].join(" ")}` }
}
