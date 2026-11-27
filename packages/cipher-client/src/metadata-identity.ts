// packages/cipher-client/src/metadata-identity.ts

import { Agent, request } from "node:http";
import type { LookupFunction } from "node:net";
import { errorClass } from "@legion/contracts/log-text";
import type { SubjectToken, TokenSource } from "./source";
import { SUBJECT_TOKEN_TYPE_GOOGLE } from "./wire";

/** Replaces the metadata server's host (and port), as Google's own client libraries read it and as
 *  the Go reader does (`internal/googleidentity`). Its value is a host, with no scheme. */
export const METADATA_HOST_VAR = "GCE_METADATA_HOST";

/** The metadata server every Google machine answers on. */
const DEFAULT_METADATA_HOST = "metadata.google.internal";

/** Mints an identity token for the instance's default service account. */
const IDENTITY_PATH = "/computeMetadata/v1/instance/service-accounts/default/identity";

/** Bounds one token fetch. The metadata server is on the host's own link, so a fetch that takes
 *  longer than this is a fault, not a slow network. */
const TIMEOUT_MS = 10_000;

/** The most of an answer that is read, so a server in the metadata server's place cannot grow this
 *  process without bound. */
const MAX_ANSWER_BYTES = 1 << 20;

/**
 * The connections this reader makes, and its reason for being: an agent of this module's own,
 * never the global one.
 *
 * `fetch` is not used here at all. Bun's `fetch` honours `$HTTP_PROXY` and `$HTTPS_PROXY`, and a
 * Node host can install a proxying global dispatcher of its own; Node 24 will also proxy
 * `node:http` through `http.globalAgent` when `NODE_USE_ENV_PROXY` is set. The metadata server's
 * address is link-local rather than loopback, so any of those would hand whatever a proxy variable
 * names this machine's identity token — a credential that acts as this machine until it expires,
 * fetched again before every renewal, so one misread variable would hand over a fresh one each
 * time. A request made through `node:http` with an agent of our own reads no proxy variable at
 * all, which is what the Go reader's bare `&http.Transport{}` does for the same reason.
 */
const agent = new Agent({ keepAlive: false });

/** The identity-token URL for `audience`, honouring {@link METADATA_HOST_VAR}. */
function tokenUrl(audience: string, env: NodeJS.ProcessEnv): string {
  const host = (env[METADATA_HOST_VAR] ?? "").trim() || DEFAULT_METADATA_HOST;
  return `http://${host}${IDENTITY_PATH}?audience=${encodeURIComponent(audience)}&format=full`;
}

/**
 * The metadata server as a text names it: by {@link METADATA_HOST_VAR} where that names it, never
 * by its value, which is a host, like a NATS server's setting, and a host cannot be told from a
 * secret pasted there by its spelling; else by Google's own host, which no setting wrote.
 */
function serverNamed(env: NodeJS.ProcessEnv): string {
  return (env[METADATA_HOST_VAR] ?? "").trim() === ""
    ? `at ${DEFAULT_METADATA_HOST}`
    : `${METADATA_HOST_VAR} names`;
}

/** One answer of the metadata server: the status it gave and the body it wrote. */
interface MetadataAnswer {
  readonly status: number;
  readonly body: string;
}

/** What {@link metadataIdentity} takes beside its audience and environment. */
export interface MetadataIdentityOptions {
  /** Resolves the metadata server's host, as `node:net` resolves a host it dials; the system's
   *  resolver (`dns.lookup`) by default. Only a test replaces it. */
  readonly lookup?: LookupFunction | undefined;
}

/** One GET through {@link agent}, answering the body as text, or throwing what went wrong. */
function get(
  url: string,
  signal: AbortSignal | undefined,
  lookup: LookupFunction | undefined
): Promise<MetadataAnswer> {
  const { promise, resolve, reject } = Promise.withResolvers<MetadataAnswer>();
  const call = request(
    url,
    {
      agent,
      method: "GET",
      headers: { "Metadata-Flavor": "Google" },
      timeout: TIMEOUT_MS,
      ...(signal === undefined ? {} : { signal }),
      ...(lookup === undefined ? {} : { lookup }),
    },
    (answer) => {
      const chunks: Buffer[] = [];
      let read = 0;
      answer.on("data", (chunk: Buffer) => {
        read += chunk.length;
        if (read <= MAX_ANSWER_BYTES) chunks.push(chunk);
      });
      answer.on("end", () =>
        resolve({ status: answer.statusCode ?? 0, body: Buffer.concat(chunks).toString("utf8") })
      );
      answer.on("error", reject);
    }
  );
  // A code, as the runtime's own errors carry one, so a text names the timeout by it (`errorClass`).
  call.on("timeout", () =>
    call.destroy(
      Object.assign(new Error(`no answer within ${TIMEOUT_MS} ms`), { code: "ETIMEDOUT" })
    )
  );
  call.on("error", reject);
  call.end();
  return promise;
}

/**
 * The identity of a machine on Google Cloud: the Google identity token its Compute Engine metadata
 * server mints for `audience`, asked for with `format=full` and fetched through no proxy.
 *
 * `format=full` is required: without it Google mints a token carrying no `google.compute_engine`
 * claims, which names no project, zone or instance, so an exchange entry pinned to either refuses
 * it. Nothing here verifies a signature, an issuer, an audience or an expiry, and nothing parses a
 * claim: that is the broker's work.
 */
export function metadataIdentity(
  audience: string,
  env: NodeJS.ProcessEnv = process.env,
  options: MetadataIdentityOptions = {}
): TokenSource {
  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      const url = tokenUrl(audience, env);
      const named = `the metadata server ${serverNamed(env)}`;
      let answer: { status: number; body: string };
      try {
        answer = await get(url, signal, options.lookup);
      } catch (error) {
        // By its class: the runtime's message repeats the host it was handed.
        throw new Error(`${named} could not be reached: ${errorClass(error)}`);
      }
      if (answer.status < 200 || answer.status > 299) {
        throw new Error(`${named} answered HTTP ${answer.status}`);
      }
      const token = answer.body.trim();
      if (token === "") {
        throw new Error(`${named} answered an empty identity token`);
      }
      return { token, tokenType: SUBJECT_TOKEN_TYPE_GOOGLE };
    },
    // A Compute Engine VM's metadata server is at Google's own address, which no setting names,
    // so the variable is named only while it is set (as the Go client's MetadataIdentity.Describe).
    describe: () =>
      (env[METADATA_HOST_VAR] ?? "").trim() === ""
        ? `this machine's Google identity token for ${audience}`
        : `this machine's Google identity token for ${audience} (${METADATA_HOST_VAR} names the metadata server)`,
  };
}
