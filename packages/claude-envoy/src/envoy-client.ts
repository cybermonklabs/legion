import type { RequestCredential } from "@legion/envoy-client/bearer"
import ky from "ky"

/** How long one send may take, the credential's trade included. */
const SEND_TIMEOUT_MS = 5_000

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  /** What the send presents (`EnabledEnvoySettings.credential`): the listener's bearer, this
   *  machine's broker credential, which signs a DPoP proof for this request, or none, which
   *  sends no `Authorization` header, for a loopback listener that asks for none. */
  readonly credential: RequestCredential | undefined
  readonly targetSession: string
  readonly message: string
  /** Ends the send early, beside its own {@link SEND_TIMEOUT_MS}. */
  readonly signal?: AbortSignal | undefined
}

/**
 * Sends one direct message. One deadline bounds the whole send, and the credential is given it
 * too (`RequestCredential.headers`): a broker credential waiting on a pod's missing registration
 * stops at it, naming the session, while its trade goes on in the background.
 */
export async function sendEnvoyMessage(options: SendEnvoyMessageOptions): Promise<void> {
  const url = new URL("/v1/messages/send", options.baseUrl)
  const timeout = AbortSignal.timeout(SEND_TIMEOUT_MS)
  const deadline =
    options.signal === undefined ? timeout : AbortSignal.any([options.signal, timeout])
  const headers =
    (await options.credential?.headers("POST", url.toString(), undefined, deadline)) ?? {}
  await ky.post(url, {
    json: {
      target_session: options.targetSession,
      message: options.message,
    },
    headers,
    retry: 0,
    timeout: false,
    signal: deadline,
  })
}
