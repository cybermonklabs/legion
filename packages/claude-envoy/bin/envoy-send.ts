import { messageFor } from "@legion/envoy-client/errors"
import { sessionConfigFile, sessionEnvoySettings } from "@legion/envoy-client/session-settings"
import { createEnvoyClient } from "@legion/envoy-client/transport"
import { configuredSessionId } from "../src/claude-session"
import { parseSendArguments } from "../src/send-arguments"

try {
  const { targetSession, message } = parseSendArguments(process.argv.slice(2))
  // The listener and bearer every command resolves: the envoy.json `envoyConfigPath("command")`
  // chooses (the ENVOY_CONFIG_FILE a session's plugin hands its shell first), with ENVOY_URL and
  // ENVOY_TOKEN_FILE or ENVOY_TOKEN over its envoy section. A send opens no NATS connection.
  // The session this command acts as, where its shell names one: a broker trade writes under it.
  const session = configuredSessionId({
    ENVOY_SESSION_ID: process.env["ENVOY_SESSION_ID"],
    CLAUDE_CODE_SESSION_ID: process.env["CLAUDE_CODE_SESSION_ID"],
  })
  const envoy = sessionEnvoySettings(sessionConfigFile("command", process.env), process.env, {
    cwd: process.cwd(),
    nats: false,
    ...(session === undefined ? {} : { session }),
  })
  if (!envoy.enabled) throw new Error(`Envoy is off: ${envoy.error ?? envoy.notice}`)
  // The transport every other client sends through. It names this session as the message's
  // source, so the listener counts the message against this session's sending limit; a shell that
  // names no session sends a blank one, which the transport leaves off the wire. A refusal is an
  // `EnvoyApiError` whose message is the listener's own sentence, which the catch below prints.
  await createEnvoyClient({ baseUrl: envoy.url, credential: envoy.credential, fetch }).send({
    sourceSessionID: session ?? "",
    targetSessionID: targetSession,
    message,
  })
  process.stdout.write(`Sent Envoy message to ${targetSession}.\n`)
} catch (error) {
  process.stderr.write(`Claude Envoy send failed: ${messageFor(error)}\n`)
  process.exitCode = 1
}
