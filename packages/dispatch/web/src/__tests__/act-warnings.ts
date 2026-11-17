import { type Mock, spyOn } from "bun:test";

// React 19's act messages, each by a piece of its text as react@19.2.8 and react-dom@19.2.8 print
// it, with what the rule calls it. The table is at the top of the file on purpose: when the rule
// throws, Bun prints the `throw` line and the five lines above it, and a count of React's own
// "not wrapped" lines over a failing run must find none of this file's.
const ACT_MESSAGES = [
  // An update outside act (`react-dom-client.development.js`, `warnIfUpdatesNotWrappedWithActDEV`),
  // and a suspended resource that finished loading outside act (`pingSuspendedRoot`).
  { kind: "update outside act", text: "not wrapped in act(" },
  // "You called act(async () => ...) without await." (`react.development.js`, `act`).
  { kind: "unawaited async act", text: "without await" },
  // "You seem to have overlapping act() calls" (`react.development.js`, `popActScope`).
  { kind: "overlapping act", text: "overlapping act() calls" },
  // "A component suspended inside an `act` scope, but the `act` call was not awaited."
  { kind: "unawaited suspense", text: "suspended inside an `act` scope" },
  // An update inside act while the act environment is off, as Testing Library leaves it inside a
  // `waitFor` (`react-dom-client.development.js`, `isConcurrentActEnvironment`).
  { kind: "act environment off", text: "not configured to support act(" },
] as const;

type ActKind = (typeof ACT_MESSAGES)[number]["kind"];

/** One act message React printed: which one, the component it named, and the code it came from. */
interface ActRecord {
  readonly component: string | undefined;
  readonly frames: readonly string[];
  readonly kind: ActKind;
}

/** Every act message printed since the last `failOnActMessages`, which alone empties it. */
const records: ActRecord[] = [];

/** The spy `quietConsoleError` made last, until it is restored through `restoreQuietConsoleError`. */
let quiet: Mock<typeof console.error> | undefined;

/** A frame of `Error.stack` as Bun writes it: `at name (path:line:col)` or `at path:line:col`. */
const STACK_FRAME = /^\s*at (?:(.+?) \()?(.+?):(\d+):\d+\)?$/;

// A frame names the code a fix wraps, so a record keeps a few. A test's own frame comes first,
// since it names the call to wrap; product code under `/web/src/` comes after it.
const KEPT_FRAMES = 3;
const WEB_SRC = "/web/src/";
const TEST_FILE = /\.test\.[^/]*$/;
const TEST_HARNESS = `${WEB_SRC}__tests__/`;

// React prints from deep inside its update path: in PendingWritesIndicator's warnings the first
// test frame lay 10 to 19 frames below `console.error`, past Bun's default limit of 10.
const STACK_DEPTH = 100;

/**
 * Records each act message React prints from here on, and still prints every call as it was
 * called, act messages included, so the log shows React's own text at the moment it was printed.
 * While a row holds the spy of `quietConsoleError`, each call reaches that spy, not this wrapper.
 * Call it once, from the preload: under `--parallel` the preload runs again for each file, with a
 * fresh `console`, so the wrapper never wraps itself.
 */
export function installActMessageRecorder(): void {
  const print = console.error;
  console.error = (...args: unknown[]) => {
    try {
      recordActMessage(args);
    } finally {
      print.apply(console, args);
    }
  };
}

/**
 * Keeps `console.error` quiet for the rest of the row: a row whose code logs on purpose uses it to
 * keep that expected output out of the log. Call it in a row's body or a `beforeEach`: the
 * preload's `beforeEach` undoes a call made in a `beforeAll` or at the top of a file before the
 * first row starts. The preload puts `console.error` back after the row's own `afterEach` hooks,
 * in the `afterEach` of `setup.ts` beside its clock restore, so a row that throws before its own
 * restore still gets it back; a row may restore it sooner with `mockRestore()` on the spy this
 * returns. Bun skips that `afterEach` when an `afterEach` before it throws, so the preload's
 * `beforeEach` also puts back a spy the row before left in place: after a row in which an
 * `afterEach` before that restore throws (the row's own, or the preload's `reportReachedNetwork`),
 * `console.error` comes back only when the next row starts, and when that row is a file's last, it
 * stays quiet through that file's `afterAll`. A second call in the same row restores the first spy
 * and returns a new one. The spy records each act message it is handed before it drops the call,
 * so the rule still fails the row that drew one. It is the only sanctioned way to silence
 * `console.error`: a replacement, or a row's own spy with a `mockImplementation`, drops React's act
 * messages before the rule can read them.
 */
export function quietConsoleError(): Mock<typeof console.error> {
  restoreQuietConsoleError();
  quiet = spyOn(console, "error").mockImplementation((...args: unknown[]) => {
    recordActMessage(args);
  });
  return quiet;
}

/**
 * Restores the spy `quietConsoleError` made last and forgets it, so a later call does nothing
 * until `quietConsoleError` makes another. A row that already called `mockRestore()` on that spy
 * loses nothing: a second restore changes nothing.
 */
export function restoreQuietConsoleError(): void {
  quiet?.mockRestore();
  quiet = undefined;
}

function recordActMessage(args: readonly unknown[]): void {
  const [text, component] = args;
  if (typeof text !== "string") return;
  const message = ACT_MESSAGES.find((each) => text.includes(each.text));
  if (message === undefined) return;
  records.push({
    component: typeof component === "string" ? component : undefined,
    frames: callerFrames(),
    kind: message.kind,
  });
}

/** The test frames and then the product frames of the stack that printed, as `path:line (name)`. */
function callerFrames(): string[] {
  const limit = Error.stackTraceLimit;
  Error.stackTraceLimit = STACK_DEPTH;
  const stack = new Error().stack ?? "";
  Error.stackTraceLimit = limit;
  const tests: string[] = [];
  const products: string[] = [];
  for (const line of stack.split("\n")) {
    const frame = STACK_FRAME.exec(line);
    if (frame === null) continue;
    const [, name, path, row] = frame;
    if (path === import.meta.path || !path.includes(WEB_SRC)) continue;
    const place = `${path.slice(path.lastIndexOf(WEB_SRC) + WEB_SRC.length)}:${row}`;
    const caller = name?.replace(/^async /, "");
    const described = caller && caller !== "<anonymous>" ? `${place} (${caller})` : place;
    (TEST_FILE.test(path) || path.includes(TEST_HARNESS) ? tests : products).push(described);
  }
  return [...tests, ...products].slice(0, KEPT_FRAMES);
}

function describeRecord({ component, frames, kind }: ActRecord): string {
  const named = component === undefined ? kind : `${kind} in ${component}`;
  return frames.length === 0 ? named : `${named} at ${frames.join(", ")}`;
}

/**
 * Fails the caller when React printed an act message since the last call, naming each: its kind,
 * and its component and the frames that printed it where React and the stack give them. The list
 * is emptied before the throw, so the next row starts with none. `where` says what the messages
 * are charged to ("in or before this row").
 */
export function failOnActMessages(where: string): void {
  if (records.length === 0) return;
  const printed = records.splice(0);
  const lines = printed.map((record) => `  ${describeRecord(record)}`);
  const message = `React printed ${printed.length} act message(s) ${where}:\n${lines.join("\n")}`;
  throw new Error(message);
}
