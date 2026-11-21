import { isFinalRefusal } from "@legion/cipher-client/token-errors"
import { holdUrlUserinfo, scrub, scrubProcessStreams } from "@legion/contracts/held-secrets"
import { URL_NOT_PRINTED } from "@legion/contracts/log-text"
import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings"
import { EnvoySendUnconfirmedError } from "@legion/envoy-client/transport"
import { configuredSessionId } from "../src/claude-session"
import { sendEnvoyMessage } from "../src/envoy-client"
import {
  IDEMPOTENCY_KEY_FLAG,
  parseSendArguments,
  type Resend,
  resendCommand,
  type SendArguments,
} from "../src/send-arguments"

/** The exit of a send that may or may not have arrived (sysexits' `EX_TEMPFAIL`), apart from a
 *  refusal's 1: a script tells the two apart by status, and resends only under the key the
 *  message names. */
const EXIT_UNCONFIRMED = 75

/** `name`'s value in this process's environment, where it is set and not empty. */
function setting(name: string): string | undefined {
  const value = process.env[name]
  return value === undefined || value === "" ? undefined : value
}

/**
 * Prints how to send the message again under `key`. Where the command can be printed as it would
 * run — every URL setting it carries is one a line may name, and the scrub of held secrets leaves
 * it whole — it is printed ready to run. Otherwise the key is printed with how to resend the
 * original, and no command: a URL setting that is not plain (`ENVOY_URL` with user info, of any
 * length) is never printed (`resendCommand`), and where the scrub would change the command (the
 * message holds a secret this process withholds) a printed copy would send something else under
 * the same key.
 */
function printResend(resend: Resend): void {
  const printed = resendCommand(resend)
  if ("command" in printed && scrub(printed.command) === printed.command) {
    const token =
      setting("ENVOY_TOKEN") === undefined
        ? ""
        : " (it presents the ENVOY_TOKEN of the shell it runs in, which this line does not print)"
    process.stderr.write(`To send it again safely, run: ${printed.command}${token}\n`)
    return
  }
  const why =
    "unprinted" in printed
      ? `the command is not printed, since it holds ${printed.unprinted} ${URL_NOT_PRINTED}.`
      : "the command is not printed because it holds a secret this process withholds, so a printed copy could not reproduce the message, and would send a different one under the same key."
  process.stderr.write(
    `To send it again safely, run envoy-send again from ${resend.cwd} with ${IDEMPOTENCY_KEY_FLAG} ${resend.idempotencyKey}, the same target and the same message, with the settings this run had: ${why}\n`,
  )
}

/**
 * The one line a send the listener stored without this machine's credential prints beside its
 * Sent line, naming why it went without: the credential's error as the transport names it where
 * the send fails for it (`messageFor`, which names no token), after what went without it. A final
 * refusal (`isFinalRefusal`: a revoked sign-in, a broker's refusal no retry cures) is named as one,
 * since every later send from this machine goes without the credential too, and a listener that
 * asks for it refuses each; its remedy is in its own words (`run: agent-secrets login`).
 */
function wentWithout(error: unknown): string {
  const why = isFinalRefusal(error)
    ? "the credential was refused for good, so no retry mints it, and a listener that asks for it refuses every send from this machine until the refusal is cured"
    : "the credential could not be minted"
  return `Claude Envoy sent the message without this machine's Envoy credential, which this listener does not ask for; ${why}: ${messageFor(error)}\n`
}

// Every line this command writes passes the scrub of the secrets it holds.
scrubProcessStreams()

let send: SendArguments | undefined
let resend: Omit<Resend, "idempotencyKey"> | undefined
try {
  send = parseSendArguments(process.argv.slice(2))
  // The listener and bearer every command resolves: the envoy.json `envoyConfigPath("command")`
  // chooses (the ENVOY_CONFIG_FILE a session's plugin hands its shell first), with ENVOY_URL and
  // ENVOY_TOKEN_FILE or ENVOY_TOKEN over its envoy section. A send opens no NATS connection.
  // The session this command acts as, where its shell names one: a broker trade writes under it.
  const session = configuredSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  })
  const file = sessionConfigFile("command", process.env)
  const envoy = await sessionEnvoySettings(file, process.env, {
    cwd: process.cwd(),
    nats: false,
    ...(session === undefined ? {} : { session }),
  })
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`)
  // What chose this send's listener and credential, which a resend carries so it goes where this
  // send went from any shell: the envoy.json read (a set ENVOY_CONFIG_FILE wins over every other
  // way of finding one), ENVOY_URL and ENVOY_TOKEN_FILE where set, and the session.
  const url = setting("ENVOY_URL")
  if (url !== undefined) holdUrlUserinfo(url)
  const settings: [string, string][] = []
  if (file.path !== null) settings.push(["ENVOY_CONFIG_FILE", file.path])
  if (url !== undefined) settings.push(["ENVOY_URL", url])
  const tokenFile = setting("ENVOY_TOKEN_FILE")
  if (tokenFile !== undefined) settings.push(["ENVOY_TOKEN_FILE", tokenFile])
  if (session !== undefined) settings.push(["ENVOY_SESSION_ID", session])
  resend = {
    script: process.argv[1] ?? "envoy-send.ts",
    cwd: process.cwd(),
    settings,
    targetSession: send.targetSession,
    message: send.message,
  }
  const sent = await sendEnvoyMessage({
    baseUrl: envoy.url,
    credential: envoy.credential,
    targetSession: send.targetSession,
    message: send.message,
    ...(send.idempotencyKey === undefined ? {} : { idempotencyKey: send.idempotencyKey }),
    sourceSession: session ?? "",
  })
  process.stdout.write(`Sent Envoy message to ${send.targetSession}.\n`)
  // Stored, so it exits 0; but a send that went without its credential says why, once.
  if (sent.withoutCredential !== undefined)
    process.stderr.write(wentWithout(sent.withoutCredential))
} catch (error) {
  // A send that may have arrived is not a failure, and nor is any failure of a resend that names
  // a key: the send under that key before it may have arrived, so a refusal now says nothing of
  // it. Both name the key, and the resend under it is safe; a run without the key would deliver
  // the message twice.
  const key =
    error instanceof EnvoySendUnconfirmedError ? error.idempotencyKey : send?.idempotencyKey
  if (send !== undefined && key !== undefined) {
    const why =
      error instanceof EnvoySendUnconfirmedError
        ? messageFor(error)
        : `this resend under idempotency_key ${key} failed (${messageFor(error)}), and the send under that key before it may have arrived`
    process.stderr.write(`Claude Envoy send not confirmed: ${why}\n`)
    const safe = error instanceof EnvoySendUnconfirmedError ? error.resendSafe : true
    if (safe && resend !== undefined) printResend({ ...resend, idempotencyKey: key })
    process.exitCode = EXIT_UNCONFIRMED
  } else {
    process.stderr.write(`Claude Envoy send failed: ${messageFor(error)}\n`)
    process.exitCode = 1
  }
}
