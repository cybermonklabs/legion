import type { RequestCredential } from "@legion/envoy-client/bearer"
import { createEnvoyClient, type KeyTakenResult } from "@legion/envoy-client/transport"

export type SendEnvoyMessageOptions = {
  readonly baseUrl: string
  /** What the send presents (`EnabledEnvoySettings.credential`): the listener's bearer, this
   *  machine's broker credential, which signs a DPoP proof for this request, or none, which
   *  sends no `Authorization` header, for a loopback listener that asks for none. */
  readonly credential: RequestCredential | undefined
  /** The session the shell names, which the message goes from (`source_session`), so the
   *  listener counts it against that session's sending limit; "" for a shell that names none,
   *  which the transport leaves off the wire. */
  readonly sourceSession?: string
  readonly targetSession: string
  readonly message: string
  /** The key the send goes under: the one an unconfirmed send named, to resend it safely. One is
   *  minted where none is given. */
  readonly idempotencyKey?: string
}

/** What a stored send went without. */
export type SentEnvoyMessage = {
  /**
   * Why the send went without its credential: the very error the credential's `headers` threw
   * for the attempt the listener stored, which a listener that asks for no credential stores as
   * it stores any. Undefined where that attempt carried the credential, an attempt before it that
   * went without included, and where the send was handed none to carry.
   */
  readonly withoutCredential: unknown
  /** Never set on a stored send: see {@link KeyTakenResult}. */
  readonly keyTaken?: undefined
}

/**
 * `credential`, every member of it as it is, with the outcome of its last `headers` kept: the
 * error it threw or rejected with, rethrown unchanged, or none where it answered headers. A
 * bearer's headers still come at once, and a broker credential's as its promise settles.
 */
function recorded(credential: RequestCredential): {
  readonly credential: RequestCredential
  readonly lastFailure: () => unknown
} {
  let failure: unknown
  const answered = (headers: Readonly<Record<string, string>>) => {
    failure = undefined
    return headers
  }
  const failed = (error: unknown): never => {
    failure = error
    throw error
  }
  return {
    credential: {
      scheme: credential.scheme,
      headers: (method, url, session, signal) => {
        try {
          const ready = credential.headers(method, url, session, signal)
          return ready instanceof Promise ? ready.then(answered, failed) : answered(ready)
        } catch (error) {
          return failed(error)
        }
      },
      ...(credential.stale === undefined
        ? {}
        : { stale: (session, presented) => credential.stale?.(session, presented) }),
      ...(credential.identity === undefined
        ? {}
        : { identity: (presented) => credential.identity?.(presented) }),
      secret: () => credential.secret(),
    },
    lastFailure: () => failure,
  }
}

/**
 * Sends one direct message through envoy-client's own send (`createEnvoyClient().send`), so the
 * `envoy-send` command keeps every rule a session's own send keeps:
 * - one idempotency key for the message, on every attempt, so the listener stores it at most
 *   once however many attempts reach it;
 * - an attempt that got no answer, a 408 or a 5xx is sent again under that key, after 250 ms;
 * - a send that may have arrived is told so (`EnvoySendUnconfirmedError`), naming the key that
 *   resends it safely, never "failed": one an attempt of which got no answer, a 5xx or a 408, which
 *   a hop in front of the listener can send after it forwarded the send;
 * - each attempt's credential waits the attempt less the request's reserve
 *   (`credentialDeadline`), and a credential that could not be minted lets the request go
 *   without it, which a listener that asks for no credential answers. A listener that wants one
 *   answers 401 at its auth gate, and the error names both halves and no resend, since nothing
 *   was stored.
 *
 * A stored send answers what it went without (`SentEnvoyMessage`), so the command can say so: a
 * sign-in revoked for good is stored without the credential by a listener that asks for none, and
 * nothing else would tell the person. A send whose idempotency key another sender's message
 * already holds stored nothing, and answers that (`KeyTakenResult`), naming that message's event
 * id: the message was not delivered, and a send under a new key delivers it.
 */
export async function sendEnvoyMessage(
  options: SendEnvoyMessageOptions,
): Promise<SentEnvoyMessage | KeyTakenResult> {
  const kept = options.credential === undefined ? undefined : recorded(options.credential)
  const client = createEnvoyClient({
    baseUrl: options.baseUrl,
    fetch: globalThis.fetch,
    credential: kept?.credential,
  })
  // The shell's session as the message's source, so the listener counts the message against that
  // session's sending limit; a shell that names no session sends a blank one, which the transport
  // leaves off the wire. A refusal is an `EnvoyApiError` whose message is the listener's own
  // sentence, which `envoy-send` prints.
  const result = await client.send({
    sourceSessionID: options.sourceSession ?? "",
    targetSessionID: options.targetSession,
    message: options.message,
    ...(options.idempotencyKey === undefined ? {} : { idempotencyKey: options.idempotencyKey }),
  })
  if (result.keyTaken !== undefined) return result
  return { withoutCredential: kept?.lastFailure() }
}
