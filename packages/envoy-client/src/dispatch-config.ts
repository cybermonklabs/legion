import { BROKER_URL_VAR, IDENTITY_VAR, type ReadTrade } from "@legion/cipher-client/environment";
import { isCleanUrl } from "@legion/contracts/log-text";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import {
  type BearerSource,
  bearerCredential,
  bearerSource,
  type FileBearer,
  fileBearer,
  type RequestCredential,
} from "./bearer";
import {
  type BrokerRoute,
  type BrokerSessionName,
  brokerCredential,
  brokerRouteConfigured,
} from "./broker-credential";
import {
  type CheckoutEnvoyConfig,
  type CommandMode,
  type EnvoyConfig,
  type EnvoyConfigEnvironment,
  httpUrl,
  missingConfigNotice,
  nonEmpty,
  offWithError,
  offWithNotice,
  type ReadEnvoyConfig,
  type SectionOff,
} from "./envoy-config";
import { messageFor } from "./errors";
import {
  DPOP_KEY_FILE_VAR,
  fileTokenCredential,
  TOKEN_FILE_VAR,
  type TokenFiles,
} from "./file-token-credential";

/** Default dispatch server base URL, matching the Go server's listen address. */
const DEFAULT_SERVER_URL = "http://localhost:8766";

/** The variables `dispatchSettings` consults, by name, over whatever else the environment holds. */
export type DispatchEnvironment = EnvoyConfigEnvironment & {
  readonly DISPATCH_TOKEN?: string | undefined;
  readonly DISPATCH_TOKEN_FILE?: string | undefined;
  readonly DISPATCH_TOKEN_BROKER?: string | undefined;
  /** The broker route's read trade (`readTradeSetting`): `1` narrows Dispatch's credential to
   *  reading, and the cut names the past time it reads at. */
  readonly DISPATCH_TOKEN_BROKER_READ?: string | undefined;
  readonly DISPATCH_TOKEN_BROKER_AS_OF?: string | undefined;
  readonly DISPATCH_PUBLIC_URL?: string | undefined;
  /** The `dispatch` command's token files (`fileTokenCredential`), which no plugin reads. */
  readonly DISPATCH_BROKER_TOKEN_FILE?: string | undefined;
  readonly DISPATCH_BROKER_DPOP_KEY_FILE?: string | undefined;
};

/** Dispatch on: its URL and the credential every request presents, and `publicUrl`, Dispatch's
 * dashboard as people open it (DISPATCH_PUBLIC_URL), set only when the variable is: a Legion pod
 * reaches Dispatch at an in-cluster `url` nobody outside the cluster can open, so every link the
 * `dispatch` command prints is resolved against `publicUrl` when it is set, and against `url`
 * otherwise. Beside the `dispatch` command's token files, its origin is also the one Dispatch
 * checks each proof's URL against: `url` is then on that origin, or its host is a Kubernetes
 * Service name and each proof names the request's path on that origin (`tokenFilesTarget`). */
export interface ActiveDispatchConfig {
  readonly enabled: true;
  readonly url: string;
  /**
   * What every request presents: the bearer a variable or a file gave (`bearerCredential`), or
   * the access token the secrets broker traded this machine's identity for, or the identity of a
   * person signed in on it, presented under the `DPoP` scheme with a fresh proof of the token's
   * key on each request (`brokerCredential`), or, for the `dispatch` command, a broker token
   * traded elsewhere that two files hold (`fileTokenCredential`). One field, so no caller can
   * present half of it.
   */
  readonly credential: RequestCredential;
  readonly publicUrl?: string;
  /**
   * Set where the link base (`publicUrl`, else `url`) is not a plain http(s) URL, so every link
   * the `dispatch` command prints is a `dispatch://` reference (`linkBaseOf`): the notice a
   * session's start prints once, naming the setting that base came from and never its value.
   */
  readonly linkNotice?: string;
}

/** Dispatch on, or off with the one notice or error that says why (`SectionOff`). */
export type DispatchConfigResolution = ActiveDispatchConfig | SectionOff;

type ValidCheckout = Extract<CheckoutEnvoyConfig, { readonly kind: "valid" }>;

/** A checkout's own server and the token written beside it, which only that server gets. Throws
 * when the checkout names no token. */
function checkoutPair(checkout: ValidCheckout, serverUrl: string): BearerSource {
  const { token } = checkout.dispatch;
  if (token === undefined) {
    throw new Error(
      `${checkout.path}: dispatch.serverUrl requires dispatch.token in the same file`
    );
  }
  return {
    kind: "file",
    token,
    field: "dispatch.token",
    path: checkout.path,
    server: serverUrl,
    checkout: true,
  };
}

/** The token a file gives when no variable does: the chosen file's `dispatch.token`. A checkout's
 * `dispatch.token` here has no `dispatch.serverUrl` beside it (`checkoutPair` takes one that does),
 * and is refused. */
function writtenToken(
  config: ReadEnvoyConfig,
  checkout: ValidCheckout | null
): FileBearer | undefined {
  if (checkout?.dispatch.token !== undefined) {
    throw new Error(
      `${checkout.path}: dispatch.token requires dispatch.serverUrl in the same file`
    );
  }
  const own = config.dispatch;
  if (own.token === undefined) return undefined;
  const server = own.serverUrl ?? DEFAULT_SERVER_URL;
  return {
    token: own.token,
    field: "dispatch.token",
    path: config.path,
    server,
    checkout: false,
  };
}

/**
 * The notice for `DISPATCH_TOKEN_FILE` set while nothing names Dispatch's server, or undefined when
 * it is not set. Legion sets `DISPATCH_TOKEN_FILE` only beside `DISPATCH_URL`, in a tmux pane and in
 * a pod alike, and a pod has no envoy.json; its session keeps one shell, where an `unset
 * DISPATCH_URL` lasts. So this notice names the variable to export again, not the missing file.
 * `DISPATCH_TOKEN` alone says nothing of the kind: it also goes with a server an envoy.json names.
 * What may name the server on this machine is still said after it: the missing file's dangling
 * link, then the unread OpenCode-era user file, last, as `missingConfigNotice` says it
 * (claude-envoy's session hook prints a notice only when it ends with that file's notice).
 */
export function lostDispatchUrlNotice(
  env: DispatchEnvironment,
  config: ReadEnvoyConfig
): string | undefined {
  if (nonEmpty(env.DISPATCH_TOKEN_FILE) === undefined) return undefined;
  const link = config.missing?.link ?? null;
  const old = config.missing?.oldUserFileNotice;
  return [
    "DISPATCH_TOKEN_FILE is set and DISPATCH_URL is not, so Dispatch is off: this shell lost the DISPATCH_URL it started with. Export DISPATCH_URL again, set to the Dispatch server that token is for",
    ...(link === null ? [] : [`${config.path} is a link to ${link}, which does not exist`]),
    ...(old === undefined ? [] : [old]),
  ].join("; ");
}

/**
 * Dispatch's URL and bearer token from `config` (`readEnvoyConfig`) and the environment. An empty
 * variable counts as unset.
 *
 * The `enabled` rule is the Envoy section's (`envoySettings`): the chosen file's
 * `dispatch.enabled: false` turns Dispatch off whatever the variables say; otherwise the section's
 * fields apply, and Dispatch is on when its server resolves: `DISPATCH_URL`, a checkout's own
 * server, the chosen file's `dispatch.serverUrl`, or the default that `dispatch.enabled: true`
 * names. A checkout's file is read only while `DISPATCH_URL` is unset: `DISPATCH_URL` replaces its
 * server, its token and its `enabled` together, so a repository can neither turn off the Dispatch
 * the session's environment configured nor send its own token to the server the environment names.
 * A checkout contributes only its `dispatch.enabled: false`, or its own server with its own token;
 * its `dispatch.enabled: true` alone turns nothing on. A checkout's file that cannot be read or is
 * not valid turns Dispatch off with its error, likewise only while `DISPATCH_URL` is unset.
 *
 * The token's source is chosen first: a checkout that names its own `dispatch.serverUrl` gives that
 * server and the `dispatch.token` written beside it, and nothing else; otherwise the token resolves
 * from `DISPATCH_TOKEN_FILE` (trimmed file contents, how the Legion daemon delivers it to a pane),
 * then `DISPATCH_TOKEN`, then the chosen file's `dispatch.token` (a checkout's token without its own
 * server is refused). A set `DISPATCH_TOKEN_FILE` that cannot be read or is blank disables Dispatch
 * with that failure as `error` and never falls back. The URL follows: `DISPATCH_URL`, else the
 * server written beside a file's token, else the chosen file's `dispatch.serverUrl`, else the
 * default.
 *
 * Same server only: the chosen file's token goes with `DISPATCH_URL` only when it names the origin
 * of the server written beside it (the default when it names none); any other server needs
 * `DISPATCH_TOKEN` or `DISPATCH_TOKEN_FILE`. A checkout's server with its own token points the
 * session at that server as that token's holder, whichever server it is, the user's own included;
 * what a checkout cannot do is take the user's token, or a variable's, anywhere. A checkout's `!`
 * token is refused; the chosen file's runs here, only once Dispatch is about to use it, unless
 * `options.commands` is `kept`: then only an outcome a run already kept in this process answers,
 * and with none Dispatch is off with an error saying the command has not run (`fileValue`).
 * Dispatch is on when its URL resolves and a credential does: one of those bearers, or — when no
 * bearer is configured at all — an access token traded at the secrets broker (`AGENT_SECRETS_URL`
 * with `AGENT_SECRETS_MACHINE_IDENTITY`, `brokerCredential`) for this machine's identity, or for
 * a person's sign-in on it where one is there, which is a DPoP-bound access token rather than a
 * bearer, traded under `options.session`. The bearers are resolved first and unchanged, so a
 * session that has one keeps behaving exactly as it did. Off, it says why: a `notice` when nothing
 * names it or a file turns it off, an `error` otherwise.
 * Nothing naming it while a token variable is set is said as the `DISPATCH_URL` the shell lost
 * (`lostDispatchUrlNotice`).
 *
 * `DISPATCH_TOKEN_BROKER=1` turns that last route from a fallback into the only one: no bearer is
 * read at all — not `DISPATCH_TOKEN`, not `DISPATCH_TOKEN_FILE`, not the chosen file's
 * `dispatch.token` — and a machine that can trade nothing turns Dispatch off with an error saying
 * so, rather than presenting whatever bearer the box happens to hold. It is a deployment's word
 * about the session it launches, which `legion controller start` sets under `dispatch_token_broker`
 * so that the operator's own envoy.json cannot write Dispatch as the operator, and beside it
 * `PERSON_SIGNIN_TRADE=off` (`@legion/cipher-client`'s `PERSON_TRADE_VAR`), so that route trades
 * this machine's identity and never the operator's sign-in. `DISPATCH_TOKEN_BROKER` unset or blank
 * reads as off, and a value other than `1` written there is a mistake in the deployment, refused
 * rather than read as on or as off.
 *
 * `DISPATCH_TOKEN_BROKER_READ=1` narrows the trade of Dispatch's credential to reading, and
 * `DISPATCH_TOKEN_BROKER_AS_OF` beside it names the cut, the past time it reads at
 * (`readTradeSetting`). Either takes the broker route as `DISPATCH_TOKEN_BROKER=1` does and reads
 * no bearer, so a shell that also holds `DISPATCH_TOKEN` never writes with it. They narrow
 * Dispatch's trade alone (`brokerCredential`'s `read`): the Envoy listener and NATS refuse a
 * read-only token and keep the session's shared client. A value either setting does not take
 * turns Dispatch off with an error naming the setting.
 *
 * `DISPATCH_BROKER_TOKEN_FILE` and `DISPATCH_BROKER_DPOP_KEY_FILE` are checked before every other
 * source (`tokenFilesTarget`): a broker token traded elsewhere and the key it is bound to, which
 * only the `dispatch` command takes (`options.tokenFiles`). Every other caller that finds either
 * set turns Dispatch off with an error that says so, rather than writing with another credential
 * beside them.
 */
export async function dispatchSettings(
  env: DispatchEnvironment,
  config: EnvoyConfig,
  options: {
    readonly commands?: CommandMode | undefined;
    /** The session a broker trade writes under (`BrokerSessionName`), for a caller that acts as
     * one. Nothing else reads it: a bearer route resolves the same whatever it says. */
    readonly session?: BrokerSessionName | undefined;
    /** True for the `dispatch` command, the one caller that takes the token files. */
    readonly tokenFiles?: boolean | undefined;
  } = {}
): Promise<DispatchConfigResolution> {
  const target = await dispatchTarget(env, config, options.tokenFiles === true);
  if (!target.enabled) return target;
  const { url, urlSetting, source } = target;
  let credential: RequestCredential;
  try {
    if (source.kind === "broker") credential = brokerCredential(env, options.session, source.read);
    else if (source.kind === "files")
      credential = fileTokenCredential(source, url, source.proofOrigin);
    else {
      credential = bearerCredential(
        source.kind === "variable"
          ? source.token
          : await fileBearer(source, url, "DISPATCH", env, options.commands)
      );
    }
  } catch (error) {
    return offWithError(messageFor(error));
  }
  const publicSetting = publicUrlSetting(env);
  if (publicSetting !== undefined && "enabled" in publicSetting) return publicSetting;
  const publicUrl = publicSetting?.written;
  const base = publicUrl === undefined ? url : withoutTrailingSlashes(publicUrl);
  const linkNotice = linkNoticeFor(
    base,
    publicUrl === undefined ? urlSetting : "DISPATCH_PUBLIC_URL"
  );
  return {
    enabled: true,
    url,
    credential,
    ...(publicUrl === undefined ? {} : { publicUrl: withoutTrailingSlashes(publicUrl) }),
    ...(linkNotice === undefined ? {} : { linkNotice }),
  };
}

/**
 * The URL `dispatchSettings` resolves from `config` and the environment, without the token's
 * value: no `!` command runs, and no outcome of one is needed; a `DISPATCH_TOKEN_FILE` is read, as
 * the resolution reads it, without holding the event loop. Undefined when Dispatch is off before
 * its token is read: nothing names it, a file turns it off or cannot be used, `DISPATCH_URL` is no
 * http(s) URL, or no token source is configured. For a caller on a deadline that holds a list read
 * from one Dispatch URL and must know whether it is the configured one while the token's command
 * has not run in its process (`readSessionAnswers`' `dispatchUrl`).
 */
export async function dispatchUrl(
  env: DispatchEnvironment,
  config: EnvoyConfig
): Promise<string | undefined> {
  const target = await dispatchTarget(env, config);
  return target.enabled ? target.url : undefined;
}

/** Where Dispatch's credential comes from: a variable, a file with the server written beside it,
 * the secrets broker, where this machine's identity, or a person's sign-in on it, is traded, with
 * the read trade the read settings ask for, or the `dispatch` command's token files, with the
 * origin each proof names where it is not the request's own (`tokenFilesTarget`). */
type TokenSource =
  | Exclude<BearerSource, { readonly kind: "none" }>
  | { readonly kind: "broker"; readonly read: ReadTrade | undefined }
  | ({ readonly kind: "files"; readonly proofOrigin?: string } & TokenFiles);

/** Dispatch on, as `dispatchTarget` resolves it. */
interface DispatchTarget {
  readonly enabled: true;
  readonly url: string;
  readonly urlSetting: string;
  readonly source: TokenSource;
}

/** The settings the token files refuse beside them: every other Dispatch credential, and every
 *  setting that would trade at the secrets broker. */
const BESIDE_TOKEN_FILES = [
  "DISPATCH_TOKEN_BROKER",
  "DISPATCH_TOKEN_BROKER_READ",
  "DISPATCH_TOKEN_BROKER_AS_OF",
  "DISPATCH_TOKEN",
  "DISPATCH_TOKEN_FILE",
  BROKER_URL_VAR,
  IDENTITY_VAR,
] as const;

/** The clause the token files' refusals open with, naming each file whose setting `env` sets:
 *  "X is set" or "X and Y are set", or undefined when neither is. A plugin's refusal
 *  (`tokenFilesTarget` with `taken` false) and `dispatch login`'s (`loginCommand`) alike. */
export function tokenFilesSet(env: DispatchEnvironment): string | undefined {
  const set = [
    ...(nonEmpty(env.DISPATCH_BROKER_TOKEN_FILE) === undefined ? [] : [TOKEN_FILE_VAR]),
    ...(nonEmpty(env.DISPATCH_BROKER_DPOP_KEY_FILE) === undefined ? [] : [DPOP_KEY_FILE_VAR]),
  ];
  if (set.length === 0) return undefined;
  return `${set.join(" and ")} ${set.length === 1 ? "is" : "are"} set`;
}

/** Each of `BESIDE_TOKEN_FILES` that `env` sets, as the token files' refusal names it: a Dispatch
 *  call's (`tokenFilesTarget`) and `dispatch login`'s (`loginCommand`) alike. */
export function settingsBesideTokenFiles(env: DispatchEnvironment): readonly string[] {
  return BESIDE_TOKEN_FILES.filter((name) => nonEmpty(env[name]) !== undefined).map(
    (name) => `${name} is set`
  );
}

/**
 * `DISPATCH_PUBLIC_URL` as written and as the URL parser reads it, undefined when it is unset or
 * empty, or a refusal naming it: it must be an http(s) URL the URL parser takes whose port, when
 * it names one, is from 1 to 65535. The daemon accepts at load only values taken here, under a
 * narrower rule (its CheckPod and this read hold to
 * packages/contracts/fixtures/dispatch-public-url.json).
 */
function publicUrlSetting(
  env: DispatchEnvironment
): { readonly written: string; readonly parsed: URL } | SectionOff | undefined {
  const written = nonEmpty(env.DISPATCH_PUBLIC_URL);
  if (written === undefined) return undefined;
  const parsed = httpUrl(written);
  if (parsed === undefined || parsed.port === "0") {
    return offWithError("DISPATCH_PUBLIC_URL must be an http or https URL");
  }
  return { written, parsed };
}

/** How a Kubernetes Service's name in its cluster ends: `<service>.<namespace>.svc.cluster.local`
 *  is the host a pod reaches the Service at. */
const SERVICE_NAME_SUFFIX = ".svc.cluster.local";

/**
 * The token files as `dispatchTarget` takes them, or undefined when neither is set. A caller that
 * does not take them (`taken` false: every plugin) is refused while either is set. The `dispatch`
 * command is refused, with every setting named, when one of the two is set alone, when
 * `DISPATCH_URL` is unset, or when any of `BESIDE_TOKEN_FILES` or a `dispatch.token` of the chosen
 * file or of the checkout's file is set beside them: the token is then its only Dispatch
 * credential, and it trades nothing. The checkout's file is read for this even with
 * `DISPATCH_URL` set, and a `!` value is found by the field's presence and never run. A checkout's
 * file that cannot be read is refused too, since whether it holds a token cannot be told. The
 * URL is `DISPATCH_URL`'s alone, and Dispatch checks each proof's URL against its public origin.
 * With `DISPATCH_PUBLIC_URL` unset, each proof names the request's own URL. With it set, its
 * origin is taken for that public origin, and the case is decided here, before the credential is
 * built: a `DISPATCH_URL` on that origin is taken as it is; one whose host is a Kubernetes Service
 * name (the parsed host ends in `.svc.cluster.local`, so a trailing dot or an IP address is none)
 * is taken with `proofOrigin` set to that origin, so each proof names the request's path there;
 * any other is refused, naming both settings and quoting neither value. A `DISPATCH_PUBLIC_URL`
 * that is not an http(s) URL is refused, as `dispatchSettings` refuses it (`publicUrlSetting`).
 * `dispatch login`, which asks the broker and keeps a login in a file, refuses while either is
 * set, before any request (`loginCommand`).
 */
function tokenFilesTarget(
  env: DispatchEnvironment,
  config: ReadEnvoyConfig,
  taken: boolean
): SectionOff | DispatchTarget | undefined {
  const set = tokenFilesSet(env);
  if (set === undefined) return undefined;
  if (!taken) {
    return offWithError(
      `${set}, and only the dispatch command reads the token files: Dispatch is off here rather than written with another credential beside them`
    );
  }
  const tokenFile = nonEmpty(env.DISPATCH_BROKER_TOKEN_FILE);
  const keyFile = nonEmpty(env.DISPATCH_BROKER_DPOP_KEY_FILE);
  const explicitUrl = nonEmpty(env.DISPATCH_URL);
  const { checkout } = config;
  const problems = [
    ...(tokenFile === undefined ? [`${TOKEN_FILE_VAR} is not set`] : []),
    ...(keyFile === undefined ? [`${DPOP_KEY_FILE_VAR} is not set`] : []),
    ...(explicitUrl === undefined ? ["DISPATCH_URL is not set"] : []),
    ...settingsBesideTokenFiles(env),
    ...(config.dispatch.token === undefined ? [] : [`dispatch.token is set in ${config.path}`]),
    ...(checkout?.kind === "valid" && checkout.dispatch.token !== undefined
      ? [`dispatch.token is set in ${checkout.path}`]
      : []),
    ...(checkout?.kind === "invalid"
      ? [`${checkout.error}, so whether it sets dispatch.token cannot be told`]
      : []),
  ];
  // The three unset settings are among `problems` too; they are tested again for their types.
  if (
    problems.length > 0 ||
    tokenFile === undefined ||
    keyFile === undefined ||
    explicitUrl === undefined
  ) {
    return offWithError(
      `${TOKEN_FILE_VAR} and ${DPOP_KEY_FILE_VAR} go together, with DISPATCH_URL, as the dispatch command's only Dispatch credential: ${problems.join("; ")}`
    );
  }
  if (config.dispatch.enabled === false) {
    return offWithNotice(`${config.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  const parsed = httpUrl(explicitUrl);
  if (parsed === undefined) {
    return offWithError("DISPATCH_URL must be an http or https URL");
  }
  const publicSetting = publicUrlSetting(env);
  if (publicSetting !== undefined && "enabled" in publicSetting) return publicSetting;
  const publicOrigin = publicSetting?.parsed.origin;
  // Set only where DISPATCH_URL is a Service name off the public origin.
  let proofOrigin: string | undefined;
  if (publicOrigin !== undefined && parsed.origin !== publicOrigin) {
    if (!parsed.hostname.endsWith(SERVICE_NAME_SUFFIX)) {
      // Neither URL is quoted: either may carry user info.
      return offWithError(
        `DISPATCH_URL is neither on DISPATCH_PUBLIC_URL's origin nor at a Kubernetes Service name (a host that ends in ${SERVICE_NAME_SUFFIX}), and beside the token files it must be one of the two: Dispatch takes only proofs that name its public origin, and the dispatch command sends such a proof to that origin or to a Service name alone, never to another server`
      );
    }
    proofOrigin = publicOrigin;
  }
  return {
    enabled: true,
    url: withoutTrailingSlashes(explicitUrl),
    urlSetting: "DISPATCH_URL",
    source: {
      kind: "files",
      tokenFile,
      keyFile,
      ...(proofOrigin === undefined ? {} : { proofOrigin }),
    },
  };
}

/** `DISPATCH_TOKEN_BROKER`'s value as a question dispatchTarget can ask: true where the deployment
 * has said this session's Dispatch credential is traded at the secrets broker and nothing else,
 * false where it says nothing, and a refusal for any other value, which is a deployment that meant
 * one of the two and wrote neither. */
function brokerRouteForced(env: DispatchEnvironment): boolean | SectionOff {
  const written = (env.DISPATCH_TOKEN_BROKER ?? "").trim();
  if (written === "") return false;
  if (written === "1") return true;
  // Its value is not quoted: a token pasted into the wrong variable would be printed.
  return offWithError(
    "DISPATCH_TOKEN_BROKER takes 1: set it to 1 where this session's Dispatch credential is traded at the secrets broker (the person signed in on this machine, else the machine's own identity), and leave it unset where a bearer is read as usual"
  );
}

/** A cut as the broker takes one (`oauthwire.FormAsOf`, its `cutForm`): an RFC 3339 time with at
 *  most six fractional digits, and `Z` or a numeric offset. */
const CUT_FORM =
  /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,6}))?(?:Z|([+-])(\d{2}):(\d{2}))$/;

/** The zero time (Go's `time.Time{}`), which the broker and every reader of a cut take for no cut
 *  at all, so the broker refuses a cut that names it. */
const ZERO_TIME = Date.parse("0001-01-01T00:00:00.000Z");

/** Whether `text` is a cut the broker takes: {@link CUT_FORM}, naming a day its month has and a
 *  time of day and an offset that exist, and not the zero time under any offset
 *  ({@link ZERO_TIME}). */
function isCut(text: string): boolean {
  const parts = CUT_FORM.exec(text);
  if (parts === null) return false;
  const [year = 0, month = 0, day = 0, hour = 0, minute = 0, second = 0] = parts
    .slice(1, 7)
    .map((part) => Number(part ?? "0"));
  const [zoneHour = 0, zoneMinute = 0] = parts.slice(9, 11).map((part) => Number(part ?? "0"));
  const daysInMonth = new Date(Date.UTC(year, month, 0)).getUTCDate();
  const exists =
    month >= 1 &&
    month <= 12 &&
    day >= 1 &&
    day <= daysInMonth &&
    hour <= 23 &&
    minute <= 59 &&
    second <= 59 &&
    zoneHour <= 23 &&
    zoneMinute <= 59;
  if (!exists) return false;
  // The instant the text names, in UTC: setUTCFullYear takes a year below 100 as written, where
  // Date.UTC would read it as one of the 1900s.
  const offset = (parts[8] === "-" ? -1 : 1) * (zoneHour * 60 + zoneMinute);
  const instant = new Date(0);
  instant.setUTCFullYear(year, month - 1, day);
  instant.setUTCHours(hour, minute - offset, second, 0);
  return Number(parts[7] ?? "0") !== 0 || instant.getTime() !== ZERO_TIME;
}

/**
 * The read trade `DISPATCH_TOKEN_BROKER_READ` and `DISPATCH_TOKEN_BROKER_AS_OF` ask Dispatch's
 * credential for: undefined where neither is set, the read trade and its cut where the first is
 * `1`, and a refusal naming the setting otherwise. `DISPATCH_TOKEN_BROKER_READ` takes `1` alone;
 * the cut goes only beside it, and is an RFC 3339 time the broker takes ({@link isCut}). The
 * broker refuses a cut later than the trade, which only its clock can tell. Neither value is
 * quoted: a token pasted into the wrong variable would be printed.
 */
function readTradeSetting(env: DispatchEnvironment): ReadTrade | undefined | SectionOff {
  const read = (env.DISPATCH_TOKEN_BROKER_READ ?? "").trim();
  const cut = (env.DISPATCH_TOKEN_BROKER_AS_OF ?? "").trim();
  if (read !== "" && read !== "1") {
    return offWithError(
      "DISPATCH_TOKEN_BROKER_READ takes 1: set it to 1 where this session's Dispatch credential is a broker token that only reads, and leave it unset where it writes"
    );
  }
  if (read === "") {
    if (cut === "") return undefined;
    return offWithError(
      "DISPATCH_TOKEN_BROKER_AS_OF names the cut a read-only token reads at, and goes only beside DISPATCH_TOKEN_BROKER_READ=1: set DISPATCH_TOKEN_BROKER_READ to 1 beside it, or unset DISPATCH_TOKEN_BROKER_AS_OF"
    );
  }
  if (cut === "") return {};
  if (!isCut(cut)) {
    return offWithError(
      "DISPATCH_TOKEN_BROKER_AS_OF is not an RFC 3339 time with at most six fractional digits and Z or a numeric offset, such as 1999-12-31T23:59:59Z"
    );
  }
  return { asOf: cut };
}

/** Dispatch as `dispatchSettings` resolves it up to its token: its URL, the setting that URL was
 * read from (`urlSetting`: `DISPATCH_URL`, `dispatch.serverUrl in <file>`, or Dispatch's default),
 * and the token's source, or off saying why. The token files come first, for a caller that takes
 * them (`tokenFiles`). Reads a `DISPATCH_TOKEN_FILE` without holding the event loop, never runs a
 * `!` command. */
async function dispatchTarget(
  env: DispatchEnvironment,
  config: EnvoyConfig,
  tokenFiles = false
): Promise<SectionOff | DispatchTarget> {
  if (config.kind === "failed") return offWithError(config.error);
  const files = tokenFilesTarget(env, config, tokenFiles);
  if (files !== undefined) return files;
  const brokerOnly = brokerRouteForced(env);
  if (typeof brokerOnly !== "boolean") return brokerOnly;
  const read = readTradeSetting(env);
  if (read !== undefined && "enabled" in read) return read;
  // The read settings take the broker route as DISPATCH_TOKEN_BROKER=1 does.
  const forced = brokerOnly || read !== undefined;
  const explicitUrl = nonEmpty(env.DISPATCH_URL);
  const checkoutFile = explicitUrl === undefined ? config.checkout : null;
  if (checkoutFile?.kind === "invalid") return offWithError(checkoutFile.error);
  const checkout = checkoutFile?.kind === "valid" ? checkoutFile : null;
  const own = config.dispatch;
  if (own.enabled === false) {
    return offWithNotice(`${config.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  if (checkout?.dispatch.enabled === false) {
    return offWithNotice(`${checkout.path} sets dispatch.enabled to false: Dispatch is off`);
  }
  // On when its address resolves: DISPATCH_URL, a checkout's own server, the chosen file's server,
  // or the default its enabled: true names. A checkout's enabled: true alone would name the default
  // server and draw a variable's token to it, so it names nothing.
  const checkoutServer = checkout?.dispatch.serverUrl;
  const addressed =
    explicitUrl !== undefined ||
    checkoutServer !== undefined ||
    own.serverUrl !== undefined ||
    own.enabled === true;
  if (!addressed) {
    const lost = lostDispatchUrlNotice(env, config);
    if (lost !== undefined) return offWithNotice(lost);
    return offWithNotice(
      config.missing === null
        ? `${config.path} names no Dispatch server, and DISPATCH_URL is not set: Dispatch is off`
        : missingConfigNotice(config.path, config.missing, "Dispatch is off")
    );
  }
  if (explicitUrl !== undefined && httpUrl(explicitUrl) === undefined) {
    return offWithError("DISPATCH_URL must be an http or https URL");
  }

  // Forced, no bearer is resolved at all: not a variable, not the chosen file's `dispatch.token`,
  // not a checkout's pair. The broker route follows below, and nothing else does.
  let source: BearerSource = { kind: "none" };
  if (!forced) {
    try {
      // A checkout's server goes only with the token written beside it.
      source =
        checkout !== null && checkoutServer !== undefined
          ? checkoutPair(checkout, checkoutServer)
          : await bearerSource(env, "DISPATCH", () => writtenToken(config, checkout));
    } catch (error) {
      return offWithError(messageFor(error));
    }
  }
  if (source.kind === "none") {
    // No bearer at all: this machine may still have an identity of its own to trade.
    let route: BrokerRoute;
    try {
      route = brokerRouteConfigured(env);
    } catch (error) {
      return offWithError(messageFor(error));
    }
    if (route.traded) {
      const brokered = withoutTrailingSlashes(explicitUrl ?? own.serverUrl ?? DEFAULT_SERVER_URL);
      const urlSetting =
        explicitUrl !== undefined
          ? "DISPATCH_URL"
          : own.serverUrl !== undefined
            ? `dispatch.serverUrl in ${config.path}`
            : DEFAULT_URL_SETTING;
      return { enabled: true, url: brokered, urlSetting, source: { kind: "broker", read } };
    }
    if (forced) {
      return offWithError(
        `${read === undefined ? "DISPATCH_TOKEN_BROKER" : "DISPATCH_TOKEN_BROKER_READ"} is set, so this session's Dispatch credential is traded at the secrets broker (the person signed in on this machine, else the machine's own identity), and ${route.gap}: Dispatch is off rather than written with whatever bearer this box holds`
      );
    }
    return offWithError(
      explicitUrl === undefined
        ? `${config.path}: dispatch is enabled and no token is: set dispatch.token, or DISPATCH_TOKEN or DISPATCH_TOKEN_FILE`
        : `DISPATCH_URL is set and no token is: set DISPATCH_TOKEN or DISPATCH_TOKEN_FILE, or dispatch.token in ${config.path}`
    );
  }
  const url = withoutTrailingSlashes(
    explicitUrl ?? (source.kind === "file" ? source.server : (own.serverUrl ?? DEFAULT_SERVER_URL))
  );
  const urlSetting =
    explicitUrl !== undefined
      ? "DISPATCH_URL"
      : source.kind === "file"
        ? `dispatch.serverUrl in ${source.path}`
        : own.serverUrl !== undefined
          ? `dispatch.serverUrl in ${config.path}`
          : DEFAULT_URL_SETTING;
  return { enabled: true, url, urlSetting, source };
}

/** What names Dispatch's default address, which no setting wrote. */
const DEFAULT_URL_SETTING = "Dispatch's default URL";

/**
 * The start notice for links a session prints as `dispatch://` references (`linkBaseOf`), or
 * undefined where the link base prints: `setting` names the setting the base came from, never its
 * value, which is what cannot be printed.
 */
function linkNoticeFor(linkBase: string, setting: string): string | undefined {
  if (isCleanUrl(linkBase)) return undefined;
  return `${setting} is not a plain http(s) URL (it carries user info, a query, a fragment, '%' or '\\'), so the links Dispatch commands print are dispatch:// references; set DISPATCH_PUBLIC_URL to a plain https URL for dashboard links`;
}

/**
 * `resolution` when it has Dispatch enabled, or null when none is configured. A file, URL, or
 * token that prevents Dispatch throws `dispatch config: <reason>` rather than reading as
 * unconfigured.
 */
export function activeDispatchSettings(
  resolution: DispatchConfigResolution
): ActiveDispatchConfig | null {
  if (resolution.enabled) return resolution;
  if (resolution.error !== null) throw new Error(`dispatch config: ${resolution.error}`);
  return null;
}
