// scripts/e2e/lib/envoy-bridge.ts: relay the sandbox repository's GitHub subjects from production
// NATS (read-only) into a live proof's own NATS (stage3-devbox-workflow.sh starts it). It
// subscribes upstream to exactly `notifications.github.<owner>.<repo>.>`, a dot in either name
// written `_` as Envoy publishes it, and republishes each message unchanged downstream; it never
// publishes upstream, and it never touches Dispatch issue subjects — the instance's scratch
// Dispatch server publishes its own, and two rigs on one issue stream admitted each other's issues
// (docs/solutions/legion).
import { type ConnectionOptions, connect, type NatsConnection, type Subscription } from "nats";
import { EnvelopeSchema } from "../../../packages/contracts/src/envelope";
import { shownSettingUrl, unshownSettingUrl } from "../../../packages/contracts/src/setting-url";
import { githubSubject } from "../../../packages/contracts/src/subject";
import { LoggableError, natsFailureReason } from "../../../packages/envoy-client/src/errors";
import { natsAuthOptions } from "../../../packages/envoy-client/src/nats-auth";
import {
  natsDialedAddress,
  natsDialFailure,
  natsServersForLog,
} from "../../../packages/envoy-client/src/nats-connection";
import { operatorNatsSeed } from "./operator-nats-seed";

/** The schemes of a NATS server's URL that the rig's NATS may be written with. */
const NATS_SCHEMES = ["nats:", "tls:"];

const requiredEnvelopeFields = [
  "event_id",
  "source",
  "source_event_id",
  "topic",
  "dedupe_key",
  "issued_at",
  "payload_summary",
  "trace_id",
] as const;

const envelopeFields: Record<string, true> = {
  event_id: true,
  source: true,
  source_event_id: true,
  source_session: true,
  topic: true,
  dedupe_key: true,
  issued_at: true,
  expires_at: true,
  payload_summary: true,
  payload: true,
  payload_ref: true,
  trace_id: true,
  sender: true,
  in_reply_to: true,
  supersedes: true,
  urgency: true,
  expects_reply: true,
};

export interface BridgeConfig {
  repository: string;
  subjects: string[];
  upstreamUrl: string;
  downstreamUrl: string;
}

export type EnvelopeValidation =
  | { ok: true; shape: "current" }
  | { ok: false; missing: string[]; extra: string[]; errors: string[] };

export function bridgeConfigFromEnvironment(
  environment: Record<string, string | undefined>
): BridgeConfig {
  const repository = environment.SMOKE_REPO?.trim() ?? "";
  // The owner half is ghrepo's own grammar for an account (packages/daemon/internal/ghrepo): a
  // letter or digit, then up to 38 letters, digits, hyphens and underscores. The underscore is
  // there because a managed user's login ends in `_<shortcode>`, and the dot this check used to
  // admit in an owner is gone, since no login holds one.
  if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,38}\/[A-Za-z0-9][A-Za-z0-9._-]*$/.test(repository)) {
    throw new Error("SMOKE_REPO must be a literal <owner>/<repo> repository");
  }
  const downstreamUrl = environment.SMOKE_RIG_NATS?.trim() ?? "";
  if (!downstreamUrl) throw new Error("SMOKE_RIG_NATS is required");
  // It may carry a user and a password, or a token as its user: one that the one function would
  // not show is refused before anything is dialed, named by its length and what to correct.
  if (shownSettingUrl(downstreamUrl, NATS_SCHEMES) === undefined) {
    throw new Error(
      `SMOKE_RIG_NATS must be a nats:// or tls:// URL with a host: ${unshownSettingUrl(downstreamUrl, NATS_SCHEMES)}`
    );
  }
  const upstreamUrl = environment.SMOKE_UPSTREAM_NATS?.trim() ?? "";
  if (!upstreamUrl) throw new Error("SMOKE_UPSTREAM_NATS is required");
  if (!upstreamNatsUrl.test(upstreamUrl) || !dialsCheckedHost(upstreamUrl)) {
    throw new Error(
      "SMOKE_UPSTREAM_NATS is not one NATS URL naming a fully-qualified host: a bare alias resolves through whatever search domain the box has; name the production Envoy NATS as the operator's envoy.json does (envoy.natsUrls, nats://<host>.<tailnet>.ts.net:4222)"
    );
  }
  const [owner, name] = repository.split("/");
  return {
    repository,
    subjects: [githubSubject(owner, name, ">")],
    upstreamUrl,
    downstreamUrl,
  };
}

// One NATS server URL: an optional scheme and user info, a host with a dot, an optional port, and
// nothing after it. The client dials whatever follows the last "://", so a path or a query could
// name a host other than the one checked here. Stage 3 holds the same pattern.
const upstreamNatsUrl =
  /^(?:[A-Za-z][A-Za-z0-9+.-]*:\/\/)?(?:([^@/?#,\s]+)@)?([A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)+)(?::\d+)?\/?$/;

// Whether the host nats.js dials (natsDialedAddress, its own hostPort) is the host the pattern
// checked. nats.js reads the URL as an http: one, which ends the host at a `\` in the user info,
// so `nats://tok\x@<fqdn>` passes the pattern and dials the bare name `tok`. The port it dials can
// differ too (it takes the first `:<digits>`, a password's included); that misroutes nothing, and
// the failure line names neither.
function dialsCheckedHost(upstreamUrl: string): boolean {
  const [, , host] = upstreamNatsUrl.exec(upstreamUrl) ?? [];
  return host !== undefined && natsDialedAddress(upstreamUrl)?.host === host.toLowerCase();
}

/** The line the bridge prints once both connections answer. Each server is named by its variable
 *  alone: either may hold a credential, and the upstream names production infrastructure. */
export function bridgeReadyLine(config: BridgeConfig): string {
  return `BRIDGE READY subjects=${config.subjects.join(",")} upstream=SMOKE_UPSTREAM_NATS downstream=SMOKE_RIG_NATS`;
}

/** The line the bridge prints when it stops on `error`: why, as a log may print it
 *  (natsFailureReason). The bridge's own refusals and its dial failures are its own lines
 *  (LoggableError, natsDialFailure), which name the upstream by its variable alone; nats.js's and
 *  the runtime's errors are printed by their code, so nothing either connection was set to, the
 *  operator's production host and password included, reaches the line. */
export function bridgeFailureLine(error: unknown, _config: BridgeConfig): string {
  return `BRIDGE UNHEALTHY ${natsFailureReason(error)}`;
}

export function envelopeValidation(data: string | Uint8Array): EnvelopeValidation {
  const text = typeof data === "string" ? data : new TextDecoder().decode(data);
  let candidate: unknown;
  try {
    candidate = JSON.parse(text);
  } catch (error) {
    return {
      ok: false,
      missing: [],
      extra: [],
      errors: [`invalid JSON: ${error instanceof Error ? error.message : String(error)}`],
    };
  }
  if (typeof candidate !== "object" || candidate === null || Array.isArray(candidate)) {
    return {
      ok: false,
      missing: [...requiredEnvelopeFields],
      extra: [],
      errors: ["envelope must be a JSON object"],
    };
  }
  const envelope = candidate as Record<string, unknown>;
  const missing = requiredEnvelopeFields.filter((field) => !(field in envelope));
  const extra = Object.keys(envelope)
    .filter((field) => !(field in envelopeFields))
    .sort();
  const parsed = EnvelopeSchema.safeParse(envelope);
  const errors = parsed.success
    ? []
    : parsed.error.issues.map((issue) => `${issue.path.join(".") || "envelope"}: ${issue.message}`);
  if (missing.length === 0 && extra.length === 0 && errors.length === 0) {
    return { ok: true, shape: "current" };
  }
  return { ok: false, missing, extra, errors };
}

function validationFailure(validation: Exclude<EnvelopeValidation, { ok: true }>): string {
  return `missing=[${validation.missing.join(",")}] extra=[${validation.extra.join(",")}] errors=[${validation.errors.join("; ")}]`;
}

async function forwardMessages(
  subscription: Subscription,
  subject: string,
  downstream: NatsConnection
): Promise<void> {
  const subjectPrefix = subject.slice(0, -1);
  let firstMessage = true;
  for await (const message of subscription) {
    if (!message.subject.startsWith(subjectPrefix)) {
      throw new LoggableError(`refusing out-of-scope upstream subject ${message.subject}`);
    }
    if (firstMessage) {
      firstMessage = false;
      const validation = envelopeValidation(message.data);
      if (!validation.ok) {
        const reason = validationFailure(validation);
        console.error(`BRIDGE UNHEALTHY first-envelope subject=${message.subject} ${reason}`);
        throw new LoggableError(`first bridged envelope is incompatible: ${reason}`);
      }
      console.log(
        `BRIDGE VALIDATION shape=${validation.shape} subject=${message.subject} bytes=${message.data.byteLength}`
      );
    }
    downstream.publishMessage(message);
    await downstream.flush();
    console.log(`BRIDGED subject=${message.subject} bytes=${message.data.byteLength}`);
  }
}

/** nats.js's `connect` with `options`, whose failure is the bridge's own line naming the server by
 *  its variable (natsDialFailure, natsServersForLog). */
async function dial(options: ConnectionOptions, named: string): Promise<NatsConnection> {
  try {
    return await connect(options);
  } catch (error) {
    throw new LoggableError(natsDialFailure(named, error));
  }
}

export async function runBridge(config: BridgeConfig): Promise<void> {
  // The production server's nkey user, when it requires one: the seed a command run from the
  // operator's shell sends to that server (operator-nats-seed.ts).
  const seed = operatorNatsSeed(
    config.upstreamUrl,
    "SMOKE_UPSTREAM_NATS",
    process.env,
    process.cwd()
  );
  const upstream = await dial(
    {
      servers: config.upstreamUrl,
      name: `legion-e2e-bridge-upstream-${config.repository}`,
      ...natsAuthOptions(seed === undefined ? { kind: "none" } : { kind: "seed", seed }),
      reconnect: true,
      maxReconnectAttempts: -1,
      reconnectTimeWait: 2_000,
    },
    natsServersForLog("SMOKE_UPSTREAM_NATS", 1)
  );
  let downstream: NatsConnection | undefined;
  try {
    downstream = await dial(
      {
        servers: config.downstreamUrl,
        name: `legion-e2e-bridge-downstream-${config.repository}`,
        reconnect: true,
        maxReconnectAttempts: -1,
        reconnectTimeWait: 2_000,
      },
      natsServersForLog("SMOKE_RIG_NATS", 1)
    );
    const activeDownstream = downstream;
    const subscriptions = config.subjects.map((subject) => upstream.subscribe(subject));
    const stop = () => {
      for (const subscription of subscriptions) subscription.unsubscribe();
    };
    process.once("SIGINT", stop);
    process.once("SIGTERM", stop);
    try {
      await upstream.flush();
      await downstream.flush();
      console.log(bridgeReadyLine(config));
      await Promise.all(
        subscriptions.map((subscription, index) =>
          forwardMessages(subscription, config.subjects[index] as string, activeDownstream)
        )
      );
    } finally {
      process.off("SIGINT", stop);
      process.off("SIGTERM", stop);
    }
  } finally {
    await Promise.allSettled([upstream.drain(), ...(downstream ? [downstream.drain()] : [])]);
  }
}

if (import.meta.main) {
  const config = bridgeConfigFromEnvironment(process.env);
  await runBridge(config).catch((error) => {
    console.error(bridgeFailureLine(error, config));
    process.exitCode = 1;
  });
}
