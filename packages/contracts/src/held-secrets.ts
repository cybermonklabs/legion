// The backstop under every rule of `log-text.ts`: the secrets this process holds, and the scrub the
// clients pass their printed surfaces through (each client's changelog names which: envoy-client's
// error and reason texts, pi-envoy's logger, notices and tool answers, claude-envoy's process
// streams, broker-client's refusal text). The rules name nothing a peer or a setting wrote; the
// scrub catches what a rule let through anyway, such as a server at the wrong URL that echoes the
// bearer it was sent inside a refusal the client prints as guidance. A line that holds no held
// secret comes back as the same string, byte for byte.

/** What stands in for each held secret, or part of one, the scrub finds. */
export const WITHHELD = "<withheld>";

/** The shortest run of a held secret the scrub takes out: 10 characters. A held value is
 *  scrubbed where it stands whole, and every run of 10 or more of its characters is too. */
export const SCRUB_RUN = 10;

/** The shortest value held: a shorter one cannot be told from ordinary words of a line, and the
 *  secrets held (tokens, keys, seeds) are far longer. */
const SHORTEST_HELD = SCRUB_RUN;

/** How long a value held with a lapse is held past it: a server that echoes a token it was sent
 *  (a broker at the wrong URL, in a refusal printed as guidance) answers within this of the
 *  token's lapse, and a process that trades a new token every few minutes holds each one this long
 *  rather than for its whole life. */
export const HELD_PAST_LAPSE_MS = 60 * 60_000;

interface Held {
  /** Each held value. */
  readonly values: Set<string>;
  /** Every run of {@link SCRUB_RUN} characters of every held value: a run of 10 or more of a held
   *  value's characters stands in a line only where one of these does. */
  readonly windows: Set<string>;
  /** When each value held with a lapse is forgotten: {@link HELD_PAST_LAPSE_MS} past its lapse,
   *  in epoch milliseconds. A value not here is held for the process's life. */
  readonly forgets: Map<string, number>;
}

/** One registry per process, whichever copy of this module a bundle carries. */
const KEY = Symbol.for("@legion/contracts/held-secrets");

function held(): Held {
  const global = globalThis as { [KEY]?: Held };
  global[KEY] ??= { values: new Set(), windows: new Set(), forgets: new Map() };
  return global[KEY];
}

/** Puts every run of {@link SCRUB_RUN} characters of `value` in `windows`. */
function addWindows(windows: Set<string>, value: string): void {
  for (let at = 0; at + SCRUB_RUN <= value.length; at += 1) {
    windows.add(value.slice(at, at + SCRUB_RUN));
  }
}

/** Forgets each value whose time `forgets` names has come, and builds the windows again from the
 *  values left: two values can share a window, so one value's windows cannot be taken out alone. */
function forgetLapsed(registry: Held, now: number): void {
  let forgot = false;
  for (const [value, at] of registry.forgets) {
    if (at > now) continue;
    registry.forgets.delete(value);
    registry.values.delete(value);
    forgot = true;
  }
  if (!forgot) return;
  registry.windows.clear();
  for (const value of registry.values) addWindows(registry.windows, value);
}

/**
 * Holds `value`, a secret this process read or was handed (a broker token, a Dispatch token, an
 * identity token, a session's JWT, the contents of a secret file), so the scrub takes it out of
 * every line printed from now on. A value shorter than 10 characters is left as it is. Returns
 * `value`, so a reader can hold what it answers in one step.
 *
 * A value that stops being good at `lapsesAt` (a token's expiry, as a `Date` or epoch
 * milliseconds) is held {@link HELD_PAST_LAPSE_MS} past it and then forgotten, so a process that
 * trades for tokens all its life holds the few of the last hours, not every one. One held with no
 * lapse is held for the process's life. Holding a value again holds it for the longer of the two:
 * for life where either says so. Each call forgets what has lapsed by then.
 */
export function holdSecret(value: string, lapsesAt?: Date | number): string {
  if (value.length < SHORTEST_HELD) return value;
  const registry = held();
  forgetLapsed(registry, Date.now());
  const forgetAt = lapsesAt === undefined ? undefined : Number(lapsesAt) + HELD_PAST_LAPSE_MS;
  if (registry.values.has(value)) {
    const was = registry.forgets.get(value);
    if (was === undefined) return value;
    if (forgetAt === undefined) registry.forgets.delete(value);
    else if (forgetAt > was) registry.forgets.set(value, forgetAt);
    return value;
  }
  registry.values.add(value);
  if (forgetAt !== undefined) registry.forgets.set(value, forgetAt);
  addWindows(registry.windows, value);
  return value;
}

/** `text` percent-decoded, or as written where it does not decode. */
function decoded(text: string): string {
  try {
    return decodeURIComponent(text);
  } catch {
    return text;
  }
}

/**
 * Holds what a configured URL's user info is: its user and its password as written and as
 * decoded, and the `Authorization: Basic` value Bun's fetch builds of them and sends to that URL
 * where no header of the caller's replaces it (the base64 of the decoded `user:password`). A
 * server at that URL that echoes the header, or the URL, prints none of them. A URL with no user
 * info, or a value that is no URL, holds nothing. Returns `url`.
 */
export function holdUrlUserinfo(url: string): string {
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return url;
  }
  if (parsed.username === "" && parsed.password === "") return url;
  const user = decoded(parsed.username);
  const password = decoded(parsed.password);
  for (const value of [parsed.username, parsed.password, user, password]) holdSecret(value);
  const pair = `${user}:${password}`;
  holdSecret(pair);
  holdSecret(btoa(String.fromCharCode(...new TextEncoder().encode(pair))));
  return url;
}

/** Forgets every held secret: for a test, which holds its own from nothing. */
export function forgetHeldSecrets(): void {
  const registry = held();
  registry.values.clear();
  registry.windows.clear();
  registry.forgets.clear();
}

/**
 * `text` with every held secret, and every run of 10 or more characters of one, replaced by
 * `<withheld>`. A run of 10 or more of a held value's characters stands in a line only where one of
 * that value's 10-character windows does, so the scan looks each of the line's windows up in the
 * one set every held value's windows were put in when it was held: linear in the line's length,
 * whatever the secrets' length. Each window found is withheld, and overlapping or touching ones
 * are one stretch. A text with none comes back as itself.
 */
export function scrub(text: string): string {
  const { windows } = held();
  if (windows.size === 0 || text.length < SCRUB_RUN) return text;
  const spans: [number, number][] = [];
  for (let at = 0; at + SCRUB_RUN <= text.length; at += 1) {
    if (!windows.has(text.slice(at, at + SCRUB_RUN))) continue;
    const end = at + SCRUB_RUN;
    const last = spans.at(-1);
    if (last !== undefined && at <= last[1]) last[1] = end;
    else spans.push([at, end]);
  }
  if (spans.length === 0) return text;
  let out = "";
  let from = 0;
  for (const [start, end] of spans) {
    out += text.slice(from, start) + WITHHELD;
    from = end;
  }
  return out + text.slice(from);
}

/** How deep {@link scrubError} follows an error's causes and fields. */
const SCRUB_DEPTH = 8;

/**
 * `error` with its message, stack and every string field of its own scrubbed in place, the
 * fields of a plain object or array it holds (an `EnvoyApiError`'s `details.responseBody`), and
 * every error it holds, its cause among them, for an error a host prints whole: Bun's console
 * prints an error's own fields beside its message. Anything that is no error is left as it is. A
 * field that cannot be written (a frozen object's) is left as it is.
 */
export function scrubError<T>(error: T): T {
  scrubErrorIn(error, new WeakSet(), 0);
  return error;
}

function scrubErrorIn(error: unknown, seen: WeakSet<object>, depth: number): void {
  if (!(error instanceof Error) || seen.has(error) || depth >= SCRUB_DEPTH) return;
  seen.add(error);
  const message = scrub(error.message);
  if (message !== error.message) error.message = message;
  if (typeof error.stack === "string") {
    const stack = scrub(error.stack);
    if (stack !== error.stack) error.stack = stack;
  }
  scrubFields(error, seen, depth);
  scrubErrorIn(error.cause, seen, depth + 1);
}

/** Each string field of `value`'s own scrubbed in place, and each error, plain object or array it
 *  holds followed; an error's message, stack and cause are its caller's. */
function scrubFields(value: object, seen: WeakSet<object>, depth: number): void {
  if (depth >= SCRUB_DEPTH) return;
  const isError = value instanceof Error;
  for (const key of Object.getOwnPropertyNames(value)) {
    if (isError && (key === "message" || key === "stack" || key === "cause")) continue;
    const descriptor = Object.getOwnPropertyDescriptor(value, key);
    if (descriptor === undefined || !("value" in descriptor)) continue;
    const field: unknown = descriptor.value;
    if (typeof field === "string") {
      const scrubbed = scrub(field);
      if (
        scrubbed !== field &&
        (descriptor.writable === true || descriptor.configurable === true)
      ) {
        Object.defineProperty(value, key, { ...descriptor, value: scrubbed });
      }
    } else if (field instanceof Error) {
      scrubErrorIn(field, seen, depth + 1);
    } else if (
      typeof field === "object" &&
      field !== null &&
      !seen.has(field) &&
      (Array.isArray(field) || Object.getPrototypeOf(field) === Object.prototype)
    ) {
      seen.add(field);
      scrubFields(field, seen, depth + 1);
    }
  }
}

type Write = (chunk: unknown, ...rest: unknown[]) => boolean;

/**
 * Passes every write to this process's stdout and stderr through the scrub, for a process of its
 * own (Claude Code's channel server and hooks): a string or a UTF-8 chunk that holds no held
 * secret is written as it came, byte for byte. Installing twice installs once.
 */
export function scrubProcessStreams(): void {
  for (const stream of [process.stdout, process.stderr]) {
    const marked = stream as typeof stream & { [KEY]?: true };
    if (marked[KEY]) continue;
    marked[KEY] = true;
    const write = stream.write.bind(stream) as Write;
    (stream as { write: Write }).write = (chunk: unknown, ...rest: unknown[]) => {
      if (typeof chunk === "string") return write(scrub(chunk), ...rest);
      if (chunk instanceof Uint8Array) {
        const text = new TextDecoder().decode(chunk);
        const scrubbed = scrub(text);
        return write(scrubbed === text ? chunk : scrubbed, ...rest);
      }
      return write(chunk, ...rest);
    };
  }
}
