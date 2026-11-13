/**
 * Going is the answer Dispatch's own server writes: a question is answered from what Dispatch
 * holds, and the answer is streamed to the browser as server-sent events. Every name, code and
 * bound of that stream is declared here once, and generated into Go (`contracts.Going*`), so the
 * server that writes a frame and the page that reads it cannot drift apart.
 *
 * The frames' shapes are written by hand here and in Go (`going_decision.go`, `going/blocks.go`,
 * `going_reads.go`), since a frame union is no OpenAPI response. `fixtures/going/` holds the two
 * together: the server's own Go test writes each frame's data there
 * (`TestTheGoingFrameFixturesAreWhatTheServerWrites`), and `going.test.ts` holds every fixture to
 * the types below key for key, so a field one side gains or loses fails a test.
 */

/** The frame carrying the next piece of the answer's text, in the order the model wrote it.
 *  The text is plain text: every ASCII punctuation byte in it is backslash-escaped, which
 *  closes markdown's own constructs, so nothing a renderer is given can become a link, an
 *  image, a tag or a code span - not by itself, and not by the end of one frame meeting the
 *  start of the next. An escape closes none of GFM's literal autolinks, which a renderer finds
 *  after it has decoded the escapes, so a word a renderer would link on its own - a `www.`
 *  host, a scheme's `://`, a `mailto:` or `xmpp:`, an address - is dropped from the answer
 *  instead, whole and however many pieces of the model's text it was written in. A `text` frame
 *  therefore never carries a link; the `item` frame is where the links of an answer live.
 *  Generated into Go as `contracts.GoingEventText`. */
export const GOING_EVENT_TEXT = "text";

/** The frame naming one thing the answer cites, so the page can link it. Its `href` is the
 *  cited item's own href, and it is the one link the stream carries: the server emits the
 *  link's visible text as escaped plain text and this frame beside it.
 *  Generated into Go as `contracts.GoingEventItem`. */
export const GOING_EVENT_ITEM = "item";

/** The frame saying the answer was cut at `GOING_ANSWER_WORD_MAX` words.
 *  Generated into Go as `contracts.GoingEventCut`. */
export const GOING_EVENT_CUT = "cut";

/** The frame saying the server did not pass the answer on as the model wrote it. `dropped` is
 *  how many pieces of it that came to: a link or image reduced to the text a reader sees of it,
 *  a dropped tag or autolink, and a word GFM would have linked on its own. A link to an item
 *  the model was given is no such change: the `item` frame carries it.
 *  Generated into Go as `contracts.GoingEventChanged`. */
export const GOING_EVENT_CHANGED = "changed";

/** The frame saying the model produced no answer text at all. An answer that kept no link is
 *  not this, and neither is one the server reduced or dropped every word of: each is shown
 *  with what it came to, and its `end` frame's `items` of 0 says it rests on no source.
 *  Generated into Go as `contracts.GoingEventNone`. */
export const GOING_EVENT_NONE = "none";

/** The frame ending a stream that answered, with what the answer came to.
 *  Generated into Go as `contracts.GoingEventEnd`. */
export const GOING_EVENT_END = "end";

/** The frame ending a stream that failed, carrying one of the `GOING_ERROR_*` codes.
 *  Generated into Go as `contracts.GoingEventError`. */
export const GOING_EVENT_ERROR = "error";

/** The frame carrying what the decision model chose before the answer is written: the two best
 *  actions, whether the bar runs the first at once, what the words are about, and how the answer
 *  is shown. It is the stream's first frame when a decision model is configured and answered,
 *  and the stream carries none otherwise. Generated into Go as `contracts.GoingEventDecision`. */
export const GOING_EVENT_DECISION = "decision";

/** The frame carrying the arguments of each action the decision offered, parsed against that
 *  action's own schema, after the answer's last text frame and before the frame that ends the
 *  stream. A stream that carries a `decision` frame and answers carries one of these.
 *  Generated into Go as `contracts.GoingEventProposal`. */
export const GOING_EVENT_PROPOSAL = "proposal";

/** The frame carrying one typed block drawn beside the answer, filled from a read the server
 *  serves, after the answer's text frames. Generated into Go as `contracts.GoingEventBlock`. */
export const GOING_EVENT_BLOCK = "block";

/** The frame saying how fresh the roster an answer read was, written when the run read it.
 *  Generated into Go as `contracts.GoingEventRoster`. */
export const GOING_EVENT_ROSTER = "roster";

/** The error code of a run the model never answered inside `GOING_RUN_MS`.
 *  Generated into Go as `contracts.GoingErrorAnswerDidNotArrive`. */
export const GOING_ERROR_ANSWER_DID_NOT_ARRIVE = "ANSWER_DID_NOT_ARRIVE";

/** The error code of a deployment with no text model configured, which is the one failure the
 *  page explains rather than retries. Generated into Go as
 *  `contracts.GoingErrorNoModelConfigured`. */
export const GOING_ERROR_NO_MODEL_CONFIGURED = "NO_MODEL_CONFIGURED";

/** The error code of a run the model service refused or failed; it names no detail, because a
 *  model service's own text is not shown to a reader. Generated into Go as
 *  `contracts.GoingErrorAnswerFailed`. */
export const GOING_ERROR_ANSWER_FAILED = "ANSWER_FAILED";

/** The name of the one tool the model is given: another search of Dispatch.
 *  Generated into Go as `contracts.GoingSearchTool`. */
export const GOING_SEARCH_TOOL = "search_dispatch";

/** The most words an answer carries; past it the stream cuts the answer and says so with a
 *  `cut` frame. Generated into Go as `contracts.GoingAnswerWordMax`. */
export const GOING_ANSWER_WORD_MAX = 120;

/** The most of one hit's own text the model is shown, in runes, so a single long document cannot
 *  fill the run's budget. Generated into Go as `contracts.GoingHitRuneMax`. */
export const GOING_HIT_RUNE_MAX = 2000;

/** The most hit text one run shows the model in total, in runes; a hit past it is still named to
 *  the model, with its kind, id and title and no body, and stays something the answer may cite.
 *  Generated into Go as `contracts.GoingRequestRuneMax`. */
export const GOING_REQUEST_RUNE_MAX = 30000;

/** The most searches one run makes, the server's own seeding search counting as the first, so the
 *  model may search twice more. Generated into Go as `contracts.GoingSearchMax`. */
export const GOING_SEARCH_MAX = 3;

/** The most calls one run makes to the model, which bounds a model that keeps asking for another
 *  search instead of answering. Generated into Go as `contracts.GoingModelCallMax`. */
export const GOING_MODEL_CALL_MAX = 4;

/** The longest question the route accepts, in UTF-16 units.
 *  Generated into Go as `contracts.GoingWordsMax`. */
export const GOING_WORDS_MAX = 2000;

/** How long one run may take, measured from the POST that started it; past it the stream ends
 *  with `GOING_ERROR_ANSWER_DID_NOT_ARRIVE`. Generated into Go as `contracts.GoingRunWindow`. */
export const GOING_RUN_MS = 30_000;

/** How long a run's rows are kept, so a reader that reconnects still finds its answer and a
 *  finished run is not kept for ever. Generated into Go as `contracts.GoingRequestTTL`. */
export const GOING_REQUEST_TTL_MS = 3_600_000;

/** The next piece of the answer's text, as plain text whose every ASCII punctuation byte is
 *  backslash-escaped, so a renderer makes no link, image, tag or code span of any of it, and
 *  with every word a renderer would link on its own dropped rather than sent. */
export interface GoingTextEvent {
  readonly event: typeof GOING_EVENT_TEXT;
  readonly data: { readonly text: string };
}

/** One thing the answer cites: its kind and id name it, `href` links it and `title` names it to a
 *  reader. This is the one frame of the stream that carries a link. `title`, `kind` and `id` are
 *  plain text, which a reader is shown as text and never read as markdown. */
export interface GoingItemEvent {
  readonly event: typeof GOING_EVENT_ITEM;
  readonly data: {
    readonly kind: string;
    readonly id: string;
    readonly href: string;
    readonly title: string;
  };
}

/** The answer was cut; `words` is how many words it carries. */
export interface GoingCutEvent {
  readonly event: typeof GOING_EVENT_CUT;
  readonly data: { readonly words: number };
}

/** The server did not pass the answer on as the model wrote it; `dropped` is how many pieces of
 *  it that came to, links and images reduced to their text among them. */
export interface GoingChangedEvent {
  readonly event: typeof GOING_EVENT_CHANGED;
  readonly data: { readonly dropped: number };
}

/** The model produced no answer text at all. */
export interface GoingNoneEvent {
  readonly event: typeof GOING_EVENT_NONE;
  readonly data: Record<string, never>;
}

/** The stream ended with an answer, of `words` words citing `items` things. */
export interface GoingEndEvent {
  readonly event: typeof GOING_EVENT_END;
  readonly data: { readonly words: number; readonly items: number };
}

/** The stream ended with a failure, named by one of the `GOING_ERROR_*` codes. */
export interface GoingErrorEvent {
  readonly event: typeof GOING_EVENT_ERROR;
  readonly data: { readonly code: string };
}

/** One action the decision model ranked: the catalogue action's name, or `none` for words that
 *  ask for nothing to change. `confidence` is the confidence the model reported for its choice,
 *  which is the number the action threshold is compared with, and is set on the first action
 *  alone: the model reports none for the runner-up. `probability` is the share the model gave
 *  that choice, null where it reported none. */
export interface GoingDecidedAction {
  readonly action: string;
  readonly confidence: number | null;
  readonly probability: number | null;
}

/** What the words are about, as the decision model chose: the item the bar's context chip names
 *  (`page`), items the words name themselves (`named`), or nothing in particular (`none`). The
 *  server fills an action's target from the chip on `page` alone. */
export type GoingDecidedContext = "page" | "named" | "none";

/** What the decision model chose before the answer was written. `actions` is the two best
 *  choices, the model's own first; `act` is true where the bar runs the first at once on submit:
 *  it is a catalogue action, its confidence is at or above the measured action threshold, and it
 *  is none of the writes that always ask. `display.chart` is true where the display choice is a
 *  chart at or above the measured chart-display threshold, so the answer draws one unasked. */
export interface GoingDecisionEvent {
  readonly event: typeof GOING_EVENT_DECISION;
  readonly data: {
    readonly actions: readonly GoingDecidedAction[];
    readonly act: boolean;
    readonly context: GoingDecidedContext;
    readonly display: {
      readonly choice: "chart" | "prose";
      readonly confidence: number;
      readonly chart: boolean;
    };
  };
}

/** The one argument an invocation still needs, which the bar asks the person for: a `text`, a
 *  `number`, one of the `enum`'s `choices`, or an `item` the search finds, `item` naming the kind
 *  searched for (`issue`, `ask`, `comment`, `document` or `message`). `field` is the action's own
 *  argument, or `@session` for a row that addresses the caller's own session where the caller's
 *  credential proved none, which the person names as text. */
export interface GoingMissingArgument {
  readonly field: string;
  readonly kind: "text" | "number" | "enum" | "item";
  readonly choices?: readonly string[];
  readonly item?: string;
}

/** One run of an action: the arguments the answer and the page filled, each checked against the
 *  action's own schema, the one argument still missing, null where none is, and whether it may
 *  run at once on submit: only where every item it names is the chip's issue or project, where the
 *  words are about the page, or is named by a reference in the person's own words (an issue key,
 *  a project key typed in upper case, an id, a `dispatch://` reference; a document's name only
 *  under an owner so named), and every route or assignee it sets is named in the words or already
 *  the issue's. Any other invocation came from text someone stored and is an offer the person
 *  picks, which the server refuses to run as `auto` (`GOING_NOT_AUTOMATIC`). `target_argument` is
 *  the argument naming the item the invocation acts on, as the row it reaches binds that item into
 *  its path, `""` for none: a write that makes its item, or one addressing the caller's own
 *  session. */
export interface GoingInvocation {
  readonly arguments: Readonly<Record<string, unknown>>;
  readonly missing: GoingMissingArgument | null;
  readonly automatic: boolean;
  readonly target_argument: string;
}

/** One action the decision offered: one invocation per item it changes, and `offered` false
 *  where no invocation survived the checks, which leaves `invocations` empty. */
export interface GoingOffer {
  readonly action: string;
  readonly invocations: readonly GoingInvocation[];
  readonly offered: boolean;
}

/** The arguments of each action the decision offered, in the decision's order. */
export interface GoingProposalEvent {
  readonly event: typeof GOING_EVENT_PROPOSAL;
  readonly data: { readonly offers: readonly GoingOffer[] };
}

/** The seven kinds of block an answer may draw. */
export type GoingBlockKind =
  | "chart"
  | "progress"
  | "table"
  | "issue_list"
  | "agent_status"
  | "metric"
  | "timeline";

/** The part of every block's spec that names the read filling it: `source` is the read tool
 *  (`read_progress`, `read_architecture`, `read_project_activity`, `read_delivery` or
 *  `read_roster`), `params` the arguments the server ran it with, and `table` the table of its
 *  answer the block's rows come from. A spec names fields and never carries a number. */
export interface GoingBlockSource {
  readonly source: string;
  readonly params: Readonly<Record<string, unknown>>;
  readonly table: string;
}

/** A chart: one series of at least two points, `x` the field along the axis (a week, a status, a
 *  component, a day) and `y` the numeric field plotted, drawn as bars or a line. */
export interface GoingChartSpec extends GoingBlockSource {
  readonly x: string;
  readonly y: string;
  readonly mark: "bar" | "line";
}

/** A table: the rows' `columns`, in order. */
export interface GoingTableSpec extends GoingBlockSource {
  readonly columns: readonly string[];
}

/** A metric: one number, the one row's `field`, named by `label`, and at most one comparison,
 *  the row's `compare` field named by `compare_label`; both absent for none. No axes, no series. */
export interface GoingMetricSpec extends GoingBlockSource {
  readonly field: string;
  readonly label: string;
  readonly compare?: string;
  readonly compare_label?: string;
}

/** Progress: one bar per row, named by its `label` field, `done` of `total`. */
export interface GoingProgressSpec extends GoingBlockSource {
  readonly label: string;
  readonly done: string;
  readonly total: string;
}

/** Fields shared by every block frame: its title, and its rows, the records the server's own
 *  read of the spec's source answered, cut to the fields the block draws. Every number a block
 *  shows is a value of these rows. */
interface GoingBlockFrame<K extends GoingBlockKind, S> {
  readonly kind: K;
  readonly title: string;
  readonly spec: S;
  readonly rows: readonly Readonly<Record<string, unknown>>[];
}

/** One block drawn beside the answer, after the answer's text frames and outside its word cut.
 *  `issue_list` rows carry `key`, `title` and `status`; `agent_status` rows `session_id`,
 *  `title`, `state`, `open_asks` and `last_activity`; `timeline` rows `at` (RFC3339), `lane`
 *  and `label`; every other kind's rows carry the fields its spec names. */
export type GoingBlockData =
  | GoingBlockFrame<"chart", GoingChartSpec>
  | GoingBlockFrame<"table", GoingTableSpec>
  | GoingBlockFrame<"metric", GoingMetricSpec>
  | GoingBlockFrame<"progress", GoingProgressSpec>
  | GoingBlockFrame<"issue_list", GoingBlockSource>
  | GoingBlockFrame<"agent_status", GoingBlockSource>
  | GoingBlockFrame<"timeline", GoingBlockSource>;

/** The frame carrying one typed block, filled from a read the server serves. */
export interface GoingBlockEvent {
  readonly event: typeof GOING_EVENT_BLOCK;
  readonly data: GoingBlockData;
}

/** How fresh the roster the answer read was: `age_ms` since Dispatch's copy of the listener's
 *  session list was read, from the live roster (`live`) or from the stored rows alone because the
 *  live roster was unavailable (`stored`). */
export interface GoingRosterEvent {
  readonly event: typeof GOING_EVENT_ROSTER;
  readonly data: { readonly age_ms: number; readonly source: "live" | "stored" };
}

/** Every frame a reader of the stream sees; `end` and `error` are the two that end it. */
export type GoingEvent =
  | GoingTextEvent
  | GoingItemEvent
  | GoingCutEvent
  | GoingChangedEvent
  | GoingNoneEvent
  | GoingDecisionEvent
  | GoingProposalEvent
  | GoingBlockEvent
  | GoingRosterEvent
  | GoingEndEvent
  | GoingErrorEvent;
