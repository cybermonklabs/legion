import { SESSION_HEARTBEAT_MS } from "@legion/contracts";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { bearerCredential, bearerSource, fileBearer, type RequestCredential } from "./bearer";
import {
  type BrokerSessionName,
  brokerCredential,
  brokerRouteConfigured,
} from "./broker-credential";
import {
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  type EnvoyFileSettings,
  fileValue,
  httpUrl,
  missingConfigNotice,
  nonEmpty,
  offWithError,
  offWithNotice,
  type SectionOff,
} from "./envoy-config";
import { messageFor } from "./errors";
import {
  brokerNatsCredential,
  NATS_AUTH_VAR,
  type NatsCredential,
  NoSessionNamedError,
} from "./nats-credential";
import { readSecretFile } from "./secret-file";

/** The listener's address when neither `envoy.url` nor `ENVOY_URL` names one. */
export const DEFAULT_ENVOY_URL = "http://127.0.0.1:9020";
// There is deliberately no NATS default: the broker's location is deployment configuration
// (`envoy.natsUrls` or `ENVOY_NATS_URL`), and any baked-in value silently misroutes every
// deployment it doesn't match. An empty natsUrls means "inbound messaging not configured" -
// consumers must degrade loudly instead of dialing an invented address.
/**
 * The interval at which a pane's heartbeat refreshes its session registration:
 * `SESSION_HEARTBEAT_MS`, declared with the arrangement it belongs to in
 * `packages/contracts/src/envoy-session.ts`. Three refreshes plus one whole listener call must
 * fall strictly short of the listener's live window (`SESSION_LIVE_WINDOW_MS`), so two whole
 * refreshes may fail and the third still completes before the session reads unresponsive. The
 * sum, the window and the call's budget are stated there; change the interval there, not here.
 */
export const DEFAULT_HEARTBEAT_MS = SESSION_HEARTBEAT_MS;
/** The floor under a pass deadline when nothing names one and the heartbeat is shorter than it. */
export const MIN_PASS_DEADLINE_MS = 30_000;
/**
 * How long a reading of the session's tmux place stands before another `tmux display-message` is
 * run for it, which is also how often the cache reads on its own between registrations
 * (`createTmuxPlaceCache`). A heartbeat and a session event landing together would otherwise
 * each ask the server, and every pane on a machine asks its one tmux server, so this is what
 * holds a session to one call a lifetime however near together or far apart its registrations
 * fall, and what keeps a shared server from being polled by every session at once. A test that
 * shortens `ENVOY_HEARTBEAT_MS` must shorten `ENVOY_TMUX_PLACE_CACHE_MS` with it, or its
 * registrations all report the first reading.
 */
export const DEFAULT_TMUX_PLACE_CACHE_MS = 30_000;
const MIN_INTERVAL_MS = 25;
// The ceiling over every resolved interval: the largest delay a host timer takes without
// overflowing its 32-bit signed field. A larger one wraps and the timer fires *at once*, which
// turns each setting into its opposite and logs nothing - a heartbeat that ticks continuously,
// and a pass deadline that abandons every queued task the moment it is queued. It is about 24.8
// days, far past any configuration anyone means.
const MAX_INTERVAL_MS = 2_147_483_647;

/**
 * How long a deregistration waits for a registration already on the wire. One that lands after
 * the DELETE writes the record back, and the session stays registered with nothing heartbeating
 * it (dispatch://INFRA-392); a listener call can take a timeout and a retry (5.25 s) to fail, so
 * this is the whole of one attempt, past which the DELETE goes out anyway — one never sent always
 * leaves the record behind, where one that races a slow registration only may.
 */
export const REGISTRATION_SETTLE_MS = 5_000;

/**
 * The whole of what a shutdown path spends settling, over the writes its chain holds and the
 * registration on the wire. `REGISTRATION_SETTLE_MS` on each of them is 10 s before the DELETE is
 * even sent, and no plugin is given that long: Oh My Pi abandons a shutdown handler after 2 s,
 * and in a Legion pane the process group has 5 s between SIGTERM and SIGKILL (`DefaultGrace`,
 * `packages/daemon/internal/shim`), shared with the agent itself. A quarter of a second covers a
 * listener round trip many times over and leaves the rest of the budget for the DELETE to reach
 * the wire.
 */
export const SHUTDOWN_SETTLE_MS = 250;

/**
 * The window the one extra registration a failed heartbeat makes waits out, before the heartbeat
 * returns to its plain interval. A registration that times out under listener load costs the
 * session a third of its live window (`SESSION_LIVE_WINDOW_MS` in `@legion/contracts`), and three
 * missed refreshes drop a running session out of every live listing (dispatch://INFRA-392), so
 * one failure buys one extra call rather than a whole interval's wait.
 */
export const HEARTBEAT_RETRY_MIN_MS = 20_000;
export const HEARTBEAT_RETRY_MAX_MS = 40_000;

/**
 * How far out that extra registration is: a fresh draw between `HEARTBEAT_RETRY_MIN_MS` and
 * `HEARTBEAT_RETRY_MAX_MS` per call, so the sessions a slow listener failed together come back
 * spread over twenty seconds instead of in one burst — the listener sees one extra call per
 * failing session per interval, never a herd on the same tick. Every plugin that heartbeats
 * draws here, so the rule is one rule (`packages/pi-envoy/extensions/envoy.ts`,
 * `packages/claude-envoy/src/envoy-channel-server.ts`). `draw` is `Math.random` unless a test
 * passes its own, so a test reads a known delay without making the spread predictable in
 * production.
 */
export function heartbeatRetryDelayMs(draw: () => number = Math.random): number {
  return Math.round(
    HEARTBEAT_RETRY_MIN_MS + draw() * (HEARTBEAT_RETRY_MAX_MS - HEARTBEAT_RETRY_MIN_MS)
  );
}

/** The interval `raw` names, when it names a usable one: a finite number above zero. Unusable is
 * undefined, which each caller replaces with its own fallback. The number is as written, before
 * the bounds a resolved interval is held to, so a notice about an adjustment names the value the
 * operator set. */
function namedInterval(raw: string | undefined): number | undefined {
  const parsed = Number(raw);
  return Number.isFinite(parsed) && parsed > 0 ? parsed : undefined;
}

/** The interval `raw` names, held to the bounds every resolved interval keeps, or `fallback`
 * when it names none. The pass deadline is not one of these: it has a floor of its own, the
 * heartbeat. */
function resolvedInterval(raw: string | undefined, fallback: number): number {
  const named = namedInterval(raw);
  return named === undefined
    ? fallback
    : Math.min(Math.max(named, MIN_INTERVAL_MS), MAX_INTERVAL_MS);
}

/** The variables `envoySettings` consults, by name, over whatever else the environment holds. */
export type EnvoyEnvironment = EnvoyConfigEnvironment & {
  readonly ENVOY_URL?: string | undefined;
  readonly ENVOY_NATS_URL?: string | undefined;
  readonly ENVOY_TOKEN?: string | undefined;
  readonly ENVOY_TOKEN_FILE?: string | undefined;
  readonly NATS_NKEY_SEED?: string | undefined;
  readonly NATS_NKEY_SEED_FILE?: string | undefined;
  readonly NATS_AUTH?: string | undefined;
  readonly ENVOY_HEARTBEAT_MS?: string | undefined;
  readonly ENVOY_PASS_DEADLINE_MS?: string | undefined;
  readonly ENVOY_TMUX_PLACE_CACHE_MS?: string | undefined;
  readonly ENVOY_INBOUND?: string | undefined;
};

/** The variable that says how a session takes its inbound messages: `nats` (what unset is) or
 *  `listener`, the listener's relay socket. `envoy.inbound` names it in the file, and the variable
 *  overrides that field. */
export const ENVOY_INBOUND_VAR = "ENVOY_INBOUND";

/** A NATS user's nkey seed, and where it came from, which an error about it names. */
export interface NatsSeed {
  readonly value: string;
  readonly source: string;
}

/** The seed the environment names: the trimmed contents of the file `NATS_NKEY_SEED_FILE` names,
 * else `NATS_NKEY_SEED`. A set variable is authoritative, and the file pointer wins over the seed:
 * an empty pointer, or a missing, unreadable or blank file, throws naming the variable and the path
 * (`readSecretFile`), never a fallback to `NATS_NKEY_SEED` or to no credential; so does a blank
 * `NATS_NKEY_SEED`. Neither set is undefined. No error carries the seed. The Go clients read the
 * same two variables (`packages/envoy/internal/bus/nkey.go`, `packages/daemon/internal/natsauth`). */
export function natsSeedFromEnvironment(env: EnvoyEnvironment): NatsSeed | undefined {
  const { NATS_NKEY_SEED_FILE: file, NATS_NKEY_SEED: plain } = env;
  if (file !== undefined) {
    if (file === "") throw new Error("NATS_NKEY_SEED_FILE is set but empty");
    return {
      value: readSecretFile("NATS_NKEY_SEED_FILE", file),
      source: "the file NATS_NKEY_SEED_FILE names",
    };
  }
  if (plain === undefined) return undefined;
  const value = plain.trim();
  if (value.length === 0) throw new Error("NATS_NKEY_SEED is set but empty");
  return { value, source: "NATS_NKEY_SEED" };
}

/** Why an Envoy with no NATS server named, from the file or `ENVOY_NATS_URL`, takes no inbound
 * messages. */
const NO_NATS_SERVER = "Envoy has no NATS server (envoy.natsUrls, ENVOY_NATS_URL)";
/** Why a caller that opens no NATS connection (`nats: false`) takes none. */
const NO_NATS_CONNECTION = "this session opens no NATS connection";

/** How a session takes inbound messages: over `natsUrls` (never empty) with `natsCredential`, the
 * nkey user a seed names, the session's broker token, or none for a server that asks for no
 * credential; through the listener's relay socket (`relaySetting`, the setting that chose it:
 * `ENVOY_INBOUND`, or `envoy.inbound in <file>`), presenting `relayCredential`, the session's
 * broker credential, the only one the socket admits (`src/listener-relay.ts`); or not at all,
 * `off` saying why, and `noSessionNamed` set where the reason is a broker token for a process that
 * names no session (`NoSessionNamedError`), which a caller with a remedy of its own branches on.
 * `natsServersSetting` names the setting the servers came from (`ENVOY_NATS_URL`, or
 * `envoy.natsUrls in <file>`), which a log names them by (`natsServersForLog`), never by their
 * text. */
export type EnvoyInbound =
  | {
      readonly natsUrls: readonly string[];
      readonly natsServersSetting: string;
      readonly natsCredential: NatsCredential;
    }
  | { readonly relaySetting: string; readonly relayCredential: RequestCredential }
  | { readonly off: string; readonly noSessionNamed?: true };

/** An Envoy a session uses: every value resolved, credentials included. */
export interface EnabledEnvoySettings {
  readonly enabled: true;
  readonly url: string;
  /**
   * What every listener call presents: the listener's bearer (`bearerCredential`), the access
   * token the secrets broker traded this machine's identity for, or the identity of a person
   * signed in on it, presented under the `DPoP` scheme with a fresh proof of the token's key on
   * each request (`brokerCredential`), or undefined for a loopback listener that asks for none.
   * One field, so no caller can present half of it.
   */
  readonly credential: RequestCredential | undefined;
  /** Inbound messages, which arrive over NATS, or through the listener's relay socket where
   * `ENVOY_INBOUND` or `envoy.inbound` says `listener`. Off with `NO_NATS_SERVER` when nothing
   * names a NATS server, with the refusal when the seed for the named ones could not be read or
   * used (the listener and its bearer stand), with the refusal of a relay socket the settings
   * cannot open, and with `NO_NATS_CONNECTION` for a caller that opens none. */
  readonly inbound: EnvoyInbound;
  readonly heartbeatMs: number;
  /** How long one tmux place reading is reused: see `DEFAULT_TMUX_PLACE_CACHE_MS`. */
  readonly tmuxPlaceCacheMs: number;
  /** The bound on one task of the pi-envoy plugin's serial queues, so a task that never settles
   * cannot hold the next heartbeat tick, session event or Legion claim behind it. The default
   * never cuts a healthy pass short: the worst legitimate pass is about one 20 s NATS dial plus a
   * few `AbortSignal.timeout(5000)` HTTP calls. Never shorter than `heartbeatMs`: an explicit
   * `ENVOY_PASS_DEADLINE_MS` below it is raised to it, so a caller that wants a short deadline
   * must shorten `ENVOY_HEARTBEAT_MS` with it. */
  readonly passDeadlineMs: number;
  /** The deadline `ENVOY_PASS_DEADLINE_MS` named when that raise happened: a usable value shorter
   * than `heartbeatMs`, which `passDeadlineMs` is that interval in place of. undefined when the
   * variable named nothing usable (the fallback was never the operator's deadline) and when what
   * it named is used as given. It reports the raise to the heartbeat alone: the bounds every
   * resolved interval is held inside are applied in silence and are not reported here. */
  readonly passDeadlineRaisedFrom: number | undefined;
}

/** No Envoy: a `notice` says why, or an `error` names what could not be read or used. */
export type DisabledEnvoySettings = SectionOff;

export type EnvoySettings = EnabledEnvoySettings | DisabledEnvoySettings;

/** A NATS server URL as two spellings of one server compare: scheme and host lower-cased, the
 * default port 4222 written when none is, a trailing `/` dropped. Anything that is not a
 * `scheme://host` URL compares as written. */
function natsServerKey(url: string): string {
  const match = /^([a-z][a-z0-9+.-]*):\/\/([^@/]*@)?(\[[^\]]*\]|[^:/]*)(?::(\d+))?(\/.*)?$/i.exec(
    url
  );
  if (match === null) return url;
  const [, scheme = "", userinfo = "", host = "", port = "4222", rest = ""] = match;
  return `${scheme.toLowerCase()}://${userinfo}${host.toLowerCase()}:${port}${withoutTrailingSlashes(rest)}`;
}

/** Throws, naming `remedy`, unless `ENVOY_NATS_URL` names no servers or exactly the section's own
 * (as a set, each compared by `natsServerKey`): a credential the file holds goes only to the
 * servers written beside it, since `ENVOY_NATS_URL` is the only way to name others. */
function requireFileServers(
  section: EnvoyFileSettings,
  namedNatsUrls: readonly string[],
  filePath: string,
  remedy: string
): void {
  if (namedNatsUrls.length === 0) return;
  const servers = new Set((section.natsUrls ?? []).map(natsServerKey));
  const named = new Set(namedNatsUrls.map(natsServerKey));
  if (named.size !== servers.size || ![...named].every((server) => servers.has(server))) {
    throw new Error(
      `ENVOY_NATS_URL names other servers than ${filePath}'s envoy.natsUrls; set ${remedy} for them`
    );
  }
}

/** The NATS user's seed: `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED`, else the section's
 * `natsSeed`, which goes only with the section's own servers (`requireFileServers`). Throws naming
 * what could not be read or used. */
function natsSeed(
  env: EnvoyEnvironment,
  section: EnvoyFileSettings,
  namedNatsUrls: readonly string[],
  filePath: string
): NatsSeed | undefined {
  const fromEnvironment = natsSeedFromEnvironment(env);
  if (fromEnvironment !== undefined || section.natsSeed === undefined) return fromEnvironment;
  requireFileServers(section, namedNatsUrls, filePath, "NATS_NKEY_SEED_FILE");
  return {
    value: fileValue(section.natsSeed, "envoy.natsSeed", filePath, env),
    source: `envoy.natsSeed in ${filePath}`,
  };
}

/**
 * The credential a session's NATS connection presents, or a throw naming why it has none it can
 * use. A seed (`natsSeed`) is presented as it always was. Otherwise `NATS_AUTH`, else the
 * section's `natsAuth` (an empty variable counts as unset), says which: `broker` is the session's
 * broker token (`brokerNatsCredential`), refused naming the gap where this machine has no broker
 * route or where the caller names no session (`noSessionNamed`: a token traded under none is the
 * machine's or the signed-in person's, not the session's), and, as the file's, going only to the
 * servers written beside it (`requireFileServers`): the token and the proof it signs would
 * otherwise go to any server `ENVOY_NATS_URL` names, which could relay that proof to the hub it
 * was meant for. `none` is no credential; any other value is
 * refused, naming the setting and never its value, which may be a credential pasted into the wrong
 * place. Unset is none, whatever
 * the machine's broker route says: that route reads only `AGENT_SECRETS_URL` and the machine's
 * identity, which says nothing of whether the hub runs an auth service (`dev-stack.sh` without
 * `--callout`, for one, exports a broker route beside a hub that runs none), and a broker token
 * presented to such a hub loses the session its inbound
 * messages. `broker` beside a seed is refused naming both, rather than one of the two silently
 * winning; `none` beside one leaves the seed the credential, as a seed is wherever it is set.
 */
function natsCredential(
  env: EnvoyEnvironment,
  section: EnvoyFileSettings,
  namedNatsUrls: readonly string[],
  filePath: string,
  session: BrokerSessionName | undefined
): NatsCredential {
  const named = nonEmpty(env.NATS_AUTH);
  const auth =
    named !== undefined
      ? { value: named, source: NATS_AUTH_VAR }
      : section.natsAuth === undefined
        ? undefined
        : { value: section.natsAuth, source: `envoy.natsAuth in ${filePath}` };
  if (auth !== undefined && auth.value !== "broker" && auth.value !== "none") {
    throw new Error(`${auth.source} is neither broker nor none`);
  }
  const seed = natsSeed(env, section, namedNatsUrls, filePath);
  if (seed !== undefined) {
    if (auth?.value === "broker") {
      throw new Error(
        `${auth.source} is ${auth.value}, and ${seed.source} names an nkey seed: set one of the two`
      );
    }
    return { kind: "seed", seed };
  }
  if (auth?.value !== "broker") return { kind: "none" };
  const route = brokerRouteConfigured(env);
  if (!route.traded) throw new Error(`${auth.source} is broker, but ${route.gap}`);
  if (named === undefined) requireFileServers(section, namedNatsUrls, filePath, "NATS_AUTH=broker");
  // No name at all refuses here; a name read from a function is read at each trade, which refuses
  // a trade the moment it reads none (`NoSessionNamedError`).
  if (session === undefined) throw new NoSessionNamedError(auth.source);
  return { kind: "broker", broker: brokerNatsCredential(env, session, auth.source) };
}

/**
 * The relay socket's inbound, which `source` chose (`ENVOY_INBOUND`, or `envoy.inbound in
 * <file>`), or why it is off. Refused beside a NATS server named for the session (`natsSetting`,
 * the setting that named it), naming both rather than one of the two silently winning; for a
 * listener credential that is not the session's broker token, which is the only one the socket
 * admits; and for a caller that names no session, whose token would be the machine's or the
 * signed-in person's, not the session the socket is for.
 */
function listenerInbound(
  source: string,
  natsSetting: string | undefined,
  credential: RequestCredential | { readonly noBrokerRoute: string },
  session: BrokerSessionName | undefined
): EnvoyInbound {
  if (natsSetting !== undefined) {
    return {
      off: `${source} is listener, and ${natsSetting} names NATS servers: set one of the two`,
    };
  }
  if ("noBrokerRoute" in credential) {
    return { off: `${source} is listener, but ${credential.noBrokerRoute}` };
  }
  if (credential.scheme !== "DPoP") {
    return {
      off: `${source} is listener, but the listener's credential is a bearer (ENVOY_TOKEN, ENVOY_TOKEN_FILE or envoy.token), and the relay socket admits only the session's broker token`,
    };
  }
  if (session === undefined) {
    return {
      off: `${source} is listener, but this process names no session for its relay socket's token`,
      noSessionNamed: true,
    };
  }
  return { relaySetting: source, relayCredential: credential };
}

/**
 * The Envoy a session uses, from `config` (`readEnvoyConfig`) and the environment.
 *
 * Envoy is configured only by the chosen file and the variables: a checkout's file has no say in
 * it, so a cloned repository never chooses where a session's deliveries come from, nor turns off
 * the Envoy the session's environment configured. The `enabled` rule is the Dispatch section's
 * (`dispatchSettings`): `envoy.enabled: false` turns Envoy off whatever the variables say;
 * otherwise the section's fields apply, and Envoy is on when its address resolves: `envoy.url` or
 * `ENVOY_URL`, `envoy.natsUrls` or `ENVOY_NATS_URL`, or the default listener `envoy.enabled: true`
 * names. With none of them, Envoy is off with a notice. Each variable overrides its own field:
 * `ENVOY_URL` the listener (`url`), `ENVOY_NATS_URL` (comma-separated) the NATS servers
 * (`natsUrls`), `ENVOY_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it) then
 * `ENVOY_TOKEN` the listener's bearer (`token`), `NATS_NKEY_SEED_FILE` then `NATS_NKEY_SEED`
 * the NATS user's seed (`natsSeed`, resolved only when there are NATS servers to use it on), and
 * `NATS_AUTH` which credential the NATS connection presents where it has no seed (`natsAuth`:
 * `broker` or `none`, `natsCredential`). An empty `ENVOY_*` or `NATS_AUTH` variable counts as
 * unset; an empty `NATS_NKEY_SEED_FILE` or `NATS_NKEY_SEED` is refused, naming it
 * (`natsSeedFromEnvironment`). A set pointer that cannot be read is refused, never a fallback:
 * `ENVOY_TOKEN_FILE`'s turns Envoy off with that `error`, and a seed's turns off inbound messages
 * alone (`inbound.off` names the refusal), since the seed is for NATS only, as does a NATS
 * credential `natsCredential` refuses.
 *
 * Same server only: the file's token goes with `ENVOY_URL` only when it names the origin of the
 * listener written in the file (or the default listener when it names none); any other listener
 * needs `ENVOY_TOKEN` or `ENVOY_TOKEN_FILE`. The file's seed goes only with the servers written in
 * the file: nkey sign-in sends no seed, but a server that is not the real one can relay the real
 * server's nonce and replay the signature. So `ENVOY_NATS_URL` gets the file's seed only when it
 * names the file's servers (as a set, compared normalised: `natsServerKey`), and is otherwise
 * refused, turning off inbound messages alone; a seed from the variables goes with whatever servers
 * resolve. The file's `!` token and seed run here, only when Envoy uses them, and a seed's failing
 * command, too, turns off inbound messages alone.
 *
 * A caller that opens no NATS connection (`nats: false`: the `dispatch` command, `claude-envoy`'s
 * `envoy-send`, pi-legion's `legion.ts` extension) takes no inbound messages and resolves no NATS
 * credential, so a seed it would never send can neither run a command nor turn it off.
 */
export function envoySettings(
  env: EnvoyEnvironment,
  config: EnvoyConfig,
  options: {
    readonly nats?: boolean | undefined;
    /** The session a broker trade writes under (`BrokerSessionName`), for a caller that acts as
     * one: the listener's credential's, and the NATS connection's where it presents the broker
     * token. A bearer or seed route resolves the same whatever it says. */
    readonly session?: BrokerSessionName | undefined;
  } = {}
): EnvoySettings {
  if (config.kind === "failed") return offWithError(config.error);
  const section = config.envoy;
  if (section.enabled === false) {
    return offWithNotice(`${config.path} sets envoy.enabled to false: Envoy is off`);
  }
  const namedUrl = nonEmpty(env.ENVOY_URL);
  const namedNatsUrls = (env.ENVOY_NATS_URL ?? "")
    .split(",")
    .map((url) => url.trim())
    .filter((url) => url.length > 0);
  const addressed =
    namedUrl !== undefined ||
    namedNatsUrls.length > 0 ||
    section.url !== undefined ||
    (section.natsUrls ?? []).length > 0 ||
    section.enabled === true;
  if (!addressed) {
    return offWithNotice(
      config.missing === null
        ? `${config.path} names no Envoy listener or NATS server, and neither ENVOY_URL nor ENVOY_NATS_URL is set: Envoy is off`
        : missingConfigNotice(config.path, config.missing, "Envoy is off")
    );
  }
  if (namedUrl !== undefined && httpUrl(namedUrl) === undefined) {
    return offWithError("ENVOY_URL must be an http or https URL");
  }
  const listener = section.url ?? DEFAULT_ENVOY_URL;
  const url = namedUrl ?? listener;
  let credential: RequestCredential | undefined;
  // Why this machine has no broker credential, where it has none and no bearer either.
  let noBrokerRoute = "";
  try {
    const source = bearerSource(env, "ENVOY", () =>
      section.token === undefined
        ? undefined
        : {
            token: section.token,
            field: "envoy.token",
            path: config.path,
            server: listener,
            checkout: false,
          }
    );
    if (source.kind === "none") {
      // No bearer: this machine's identity, or a person's sign-in on it, traded at the broker, is
      // what the listener checks at the broker's introspection route. A listener that asks for no
      // credential is unaffected — it reads the request's identity, or none, the same way, and a
      // trade that fails leaves its calls standing (`createEnvoyClient`, transport.ts).
      const route = brokerRouteConfigured(env);
      credential = route.traded ? brokerCredential(env, options.session) : undefined;
      if (!route.traded) noBrokerRoute = route.gap;
    } else {
      credential = bearerCredential(
        source.kind === "variable" ? source.token : fileBearer(source, url, "ENVOY", env)
      );
    }
  } catch (error) {
    return offWithError(messageFor(error));
  }
  const natsUrls = namedNatsUrls.length > 0 ? namedNatsUrls : (section.natsUrls ?? []);
  // How the session takes inbound messages: `ENVOY_INBOUND`, else the file's `envoy.inbound`, an
  // empty variable counting as unset. Unset, and `nats`, are NATS, resolved as they always were.
  const namedInbound = nonEmpty(env.ENVOY_INBOUND);
  const choice =
    namedInbound !== undefined
      ? { value: namedInbound, source: ENVOY_INBOUND_VAR }
      : section.inbound === undefined
        ? undefined
        : { value: section.inbound, source: `envoy.inbound in ${config.path}` };
  let inbound: EnvoyInbound;
  if (options.nats === false) inbound = { off: NO_NATS_CONNECTION };
  else if (choice !== undefined && choice.value !== "nats" && choice.value !== "listener") {
    // Never echoed: a credential pasted into the wrong setting would be.
    inbound = { off: `${choice.source} is neither nats nor listener` };
  } else if (choice?.value === "listener") {
    const natsSetting =
      namedNatsUrls.length > 0
        ? "ENVOY_NATS_URL"
        : (section.natsUrls ?? []).length > 0
          ? `envoy.natsUrls in ${config.path}`
          : undefined;
    inbound = listenerInbound(
      choice.source,
      natsSetting,
      credential ?? { noBrokerRoute },
      options.session
    );
  } else if (natsUrls.length === 0) inbound = { off: NO_NATS_SERVER };
  else {
    try {
      inbound = {
        natsUrls,
        natsServersSetting:
          namedNatsUrls.length > 0 ? "ENVOY_NATS_URL" : `envoy.natsUrls in ${config.path}`,
        natsCredential: natsCredential(env, section, namedNatsUrls, config.path, options.session),
      };
    } catch (error) {
      inbound =
        error instanceof NoSessionNamedError
          ? { off: messageFor(error), noSessionNamed: true }
          : { off: messageFor(error) };
    }
  }
  const heartbeatMs = resolvedInterval(env.ENVOY_HEARTBEAT_MS, DEFAULT_HEARTBEAT_MS);
  const tmuxPlaceCacheMs = resolvedInterval(
    env.ENVOY_TMUX_PLACE_CACHE_MS,
    DEFAULT_TMUX_PLACE_CACHE_MS
  );
  const namedPassDeadlineMs = namedInterval(env.ENVOY_PASS_DEADLINE_MS);
  // The pass deadline is never shorter than the heartbeat: a shorter one abandons every pass
  // before the next tick, so the heartbeat's `healing` latch — and the registration backstop that
  // runs under it (`ensureHeartbeat`, packages/pi-envoy/extensions/envoy.ts) — would be false at
  // every tick, while a pass wedged before `registerSession` never registers at all. A caller that
  // wants a short deadline must shorten `ENVOY_HEARTBEAT_MS` with it. `MIN_INTERVAL_MS` has
  // nothing to do here: the result is at least `heartbeatMs`, which it is already floored by.
  // `MAX_INTERVAL_MS` over the `max` is the whole ceiling: the other arm is already under it.
  // The raise is reported as `passDeadlineRaisedFrom`, so the start notice reads the decision
  // made here instead of making it again from the variable (`startNotice`, session-settings.ts).
  return {
    enabled: true,
    url: withoutTrailingSlashes(url),
    credential,
    inbound,
    heartbeatMs,
    tmuxPlaceCacheMs,
    passDeadlineMs: Math.min(
      Math.max(namedPassDeadlineMs ?? MIN_PASS_DEADLINE_MS, heartbeatMs),
      MAX_INTERVAL_MS
    ),
    passDeadlineRaisedFrom:
      namedPassDeadlineMs !== undefined && namedPassDeadlineMs < heartbeatMs
        ? namedPassDeadlineMs
        : undefined,
  };
}
