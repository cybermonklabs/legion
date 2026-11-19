import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { act, cleanup, type RenderHookResult, renderHook } from "@testing-library/react";

import type { Issue } from "../../api/types";
import {
  type IssueDrafts,
  type IssueUpdateInput,
  type IssueUpdateMutation,
  useIssueDrafts,
} from "./useIssueDrafts";

// The title draft of both issue headers is one hook, and the bugs found in it have each been one
// particular order of typing, saving, escaping and answers from Dispatch. Rather than wait for the
// next order to be found by hand, this file plays every order up to five events long and checks,
// after every event, the seven things decisions 316, 324 and 332 say a reader can rely on. The
// orders a review or a sweep of the pages found come last, each as its own test, so a fix can be
// read against them by name.

const startIssue: Issue = {
  advances: [],
  advances_revision: 0,
  assignee: null,
  blocked_by: [],
  claim: null,
  closed_at: null,
  components: { mode: "inherit", ids: [], unknown: [], reason: null, inherited_from: null },
  created_at: "2026-09-09T00:00:00Z",
  created_by: { id: "alice", kind: "user" },
  decisions_artifact_id: "",
  external_links: [],
  key: "CORE-1",
  labels: [],
  last_seq: 250,
  number: 1,
  parent: null,
  plan_artifact_id: "",
  primary_artifact_id: "artifact-1",
  priority: null,
  project: "CORE",
  rank: "U",
  repo: null,
  resolved_repo: null,
  route: null,
  status: "todo",
  title: "Review the spec",
  updated_at: "2026-09-09T00:00:00Z",
  verification_artifact_id: "",
};

interface HookProps {
  issue: Issue;
  updateIssue: IssueUpdateMutation;
}

/**
 * One mounted hook, the fake mutation it writes through, and the issue its prop holds. The fake
 * records each write in order and never answers by itself: when a write lands or is refused is the
 * caller's choice, since the orders of those answers are what this file is about.
 */
class Session {
  readonly received: IssueUpdateInput[] = [];
  readonly updateIssue: IssueUpdateMutation = {
    mutate: (input) => {
      this.received.push(input);
      this.updateIssue.variables = input;
    },
    variables: undefined,
  };
  issue = startIssue;
  /** The write the fake last received that has not landed or been refused. */
  out: IssueUpdateInput | null = null;
  private readonly hook: RenderHookResult<IssueDrafts, HookProps>;

  constructor() {
    this.hook = renderHook((props: HookProps) => useIssueDrafts(props.issue, props.updateIssue), {
      initialProps: { issue: this.issue, updateIssue: this.updateIssue },
    });
  }

  get drafts(): IssueDrafts {
    return this.hook.result.current;
  }

  /** Runs one step inside `act` and returns the writes the fake received during it. */
  step(action: () => void): IssueUpdateInput[] {
    const before = this.received.length;
    act(action);
    const sent = this.received.slice(before);
    const last = sent.at(-1);
    if (last !== undefined) this.out = last;
    return sent;
  }

  type(value: string): IssueUpdateInput[] {
    return this.step(() => this.drafts.writeTitle(value));
  }

  /** The end of an edit: a blur or Enter. The new header's switch or page hide, which sends the
   *  draft and leaves the edit open, is `hide`. */
  end(): IssueUpdateInput[] {
    return this.step(() => this.drafts.requestTitleSubmit());
  }

  escape(): IssueUpdateInput[] {
    return this.step(() => this.drafts.discardTitle());
  }

  /**
   * Whether a hide asks the hook for the draft: a switch to another window, or a hide of the
   * page, with the new header's editor open sends the editor's draft and leaves the edit open
   * (`sendAway` in `IssueTitleRow.tsx`). It asks for a draft that is not empty and is changed,
   * as `sendAway` does; a title queued behind a write while the draft is typed back asks for
   * nothing, and stays the reader's last ended choice (decision 324). The title row's own guard
   * against sending a draft twice (`went`) is the row's, which the header's cases hold.
   */
  hideAsks(): boolean {
    const { drafts } = this;
    return drafts.title.trim() !== "" && drafts.titleDirty;
  }

  hide(): IssueUpdateInput[] {
    return this.step(() => {
      if (this.hideAsks()) this.drafts.requestTitleSubmit();
    });
  }

  /** The write out lands: the issue takes the title it carried, then Dispatch's answer arrives. */
  land(): IssueUpdateInput[] {
    const input = this.takeOut();
    const shown =
      input.title === undefined ? [] : this.showIssue({ ...this.issue, title: input.title });
    const next = this.issue;
    const answered = this.step(() => {
      this.drafts.onIssueSuccess(next, input);
      this.drafts.onIssueSettled(null);
    });
    return [...shown, ...answered];
  }

  refuse(): IssueUpdateInput[] {
    this.takeOut();
    return this.step(() => this.drafts.onIssueSettled(new Error("refused")));
  }

  otherReader(title: string): IssueUpdateInput[] {
    return this.showIssue({ ...this.issue, title });
  }

  /**
   * Another field's write lands carrying another reader's title, as the new header's status pick
   * does. Its answer reaches the hook, but it is not the hook's own write, so nothing settles.
   */
  otherFieldLands(title: string): IssueUpdateInput[] {
    const next = { ...this.issue, title, status: "in_progress" };
    const answered = this.step(() => this.drafts.onIssueSuccess(next, { status: "in_progress" }));
    return [...answered, ...this.showIssue(next)];
  }

  titlesSent(): string[] {
    return this.received.flatMap((input) => (input.title === undefined ? [] : [input.title]));
  }

  private takeOut(): IssueUpdateInput {
    const input = this.out;
    if (input === null) throw new Error("no write is out");
    this.out = null;
    return input;
  }

  private showIssue(next: Issue): IssueUpdateInput[] {
    this.issue = next;
    return this.step(() => this.hook.rerender({ issue: next, updateIssue: this.updateIssue }));
  }
}

const alphabet = [
  "typeNew",
  "typeServer",
  "typeOut",
  "typeMore",
  "saveNew",
  "end",
  "hide",
  "escape",
  "land",
  "refuse",
  "otherReader",
  "otherFieldLands",
] as const;
type EventName = (typeof alphabet)[number];
type Invariant = "I1" | "I2" | "I3" | "I4" | "I5" | "I6" | "I7";

interface Finding {
  invariant: Invariant;
  read: string;
}

interface TitleWrite {
  title: string;
  refused: boolean;
}

/** The events that bring the field a value of the issue's or of a write's, not the reader's. */
const answers: Partial<Record<EventName, true>> = {
  land: true,
  refuse: true,
  otherReader: true,
  otherFieldLands: true,
};

/**
 * A session and a small model of its reader: what the field holds by their own record, what they
 * last chose and still wait to see sent, what they put away, and what has been sent, which is
 * what the invariants are judged against. The model never takes a choice from the hook's draft:
 * a hook that put a wrong value in the draft would have it counted as the reader's choice.
 */
class Reader {
  readonly session = new Session();
  /**
   * The reader's last ended choice (decision 324): what the field held by their own record
   * (`shown`) at the last end, or hide, that chose a title. An end that chose nothing, an escape,
   * and its send withdraw it; typing ends no edit, so it leaves it as it is.
   */
  private chosen: string | null = null;
  /**
   * The reader's last ended choice while it has not been sent (I5). A send of it clears it, and
   * so do a later end, which chooses again or chooses nothing, a later hide that chooses again,
   * an escape, and the title it is judged against - the title out while one is, else the
   * issue's - becoming it, as a landing, a refusal or another reader can make it, which leaves it
   * nothing to send. The issue's old title, typed back behind a write out, is still owed: that
   * write is about to replace it.
   */
  private owed: string | null = null;
  /**
   * Whether the line of writes stopped at a refusal: the hook drains nothing after one (decision
   * 315), so a choice queued behind the refused write waits for the reader's next end or hide. A
   * landing starts the line again.
   */
  private stopped = false;
  /** Whether the editor is open: from a type event until the next end or escape. A hide leaves
   *  it as it is, as the new header's switch leaves the edit open. */
  private editing = false;
  /**
   * The reader's own choice the field may show: the text of the last type event, or what an
   * escape, or an end that chose nothing, put back while a write was out; none after one of
   * those with nothing out, nor once a write that carried it, sent after it was typed, has
   * landed, since the issue then holds it. By the same rule it is none once the issue's title
   * comes to be the reader's owed choice and it is that choice, untyped since they chose it, as a
   * refusal or another reader can make it: a hide is an end that leaves the editor open, and once
   * the reader's ended choice is the issue's title the open editor follows the issue, as it does
   * once a hide's write lands. Text typed after the choice stays theirs, whatever it is.
   */
  lastTyped: string | null = null;
  /**
   * Whether `lastTyped` went in a write after the reader typed it. Typed again after its write
   * went (the title out typed back), it is the reader's open text, and that write's landing
   * leaves it the reader's.
   */
  private typedSent = false;
  /** Whether the reader typed since the last end, or hide that chose a title: their text then is
   *  their open text, not the choice they ended. */
  private typedSinceChoice = false;
  /** The drafts an escape put away, until the reader types one of them again. */
  private readonly discarded = new Set<string>();
  private lastTitleWrite: TitleWrite | null = null;
  private titleWriteOut: TitleWrite | null = null;
  /** What the event being played broke before any write it sent: I7 at an end or a hide. */
  private noted: Finding[] = [];
  private typed = 0;
  private others = 0;
  private picks = 0;

  applies(event: EventName): boolean {
    switch (event) {
      case "typeOut":
        return this.session.out?.title !== undefined;
      case "land":
      case "refuse":
        return this.session.out !== null;
      default:
        return true;
    }
  }

  /**
   * Plays one event and returns what it broke: I7 at an end or a hide, I1, I2 and I3 on each
   * write it sent, and I5 and I6 once it is over.
   */
  play(event: EventName): Finding[] {
    // A save is a fresh title typed and the edit ended, each played and judged as that event is
    // alone. Two saves queue a title behind the write out, which takes four single events, so
    // a sequence of five can still type on or type back after them and let the write land.
    if (event === "saveNew") return [...this.play("typeNew"), ...this.play("end")];
    this.noted = [];
    const sent = this.apply(event);
    const judged = sent.flatMap((write) => this.judge(write));
    return [...this.noted, ...judged, ...this.afterEvent(event)];
  }

  /** What the field holds by the reader's own record: the text they last typed while it is
   *  theirs, else the title of the write out, else the issue's title. */
  private shown(): string {
    const { session } = this;
    return (this.lastTyped ?? session.out?.title ?? session.issue.title).trim();
  }

  private apply(event: Exclude<EventName, "saveNew">): IssueUpdateInput[] {
    const { session } = this;
    switch (event) {
      case "typeNew":
        this.typed += 1;
        return this.type(`t${this.typed}`);
      case "typeServer":
        return this.type(session.issue.title);
      case "typeOut":
        return this.type(session.out?.title ?? "");
      case "typeMore":
        // One character onto the end of the draft, as a reader goes on writing without ending
        // the edit.
        return this.type(`${session.drafts.title}x`);
      case "end":
        this.checkField();
        this.choose();
        this.editing = false;
        return session.end();
      case "hide": {
        // An end that leaves the editor as it is, judged by the reader's own record: it chooses
        // what the field holds when that is a title other than the one it is judged against, and
        // otherwise changes nothing. The hook is asked as the title row asks it, whatever the
        // record says, so a hook that disagrees leaves the reader's choice unsent, which I5 finds.
        this.checkField();
        const shown = this.shown();
        const against = (session.out?.title ?? session.issue.title).trim();
        if (shown !== "" && shown !== against) this.choose();
        return session.hide();
      }
      case "escape": {
        const before = session.drafts.title.trim();
        this.chosen = null;
        this.owed = null;
        this.editing = false;
        this.putBack();
        const sent = session.escape();
        if (session.drafts.title.trim() !== before) this.discarded.add(before);
        return sent;
      }
      case "land": {
        this.titleWriteOut = null;
        this.stopped = false;
        // A choice the landing write carried, sent after the reader typed it, is the issue's now:
        // a field left on it once another reader changes the title shows a stale title, not the
        // reader's own.
        const landed = session.out?.title;
        if (this.typedSent && landed !== undefined && landed === this.lastTyped?.trim()) {
          this.lastTyped = null;
        }
        return session.land();
      }
      case "refuse":
        if (this.titleWriteOut !== null) this.titleWriteOut.refused = true;
        this.titleWriteOut = null;
        this.stopped = true;
        return session.refuse();
      case "otherReader":
        this.others += 1;
        return session.otherReader(`o${this.others}`);
      case "otherFieldLands":
        this.picks += 1;
        return session.otherFieldLands(`p${this.picks}`);
    }
  }

  private type(value: string): IssueUpdateInput[] {
    this.lastTyped = value;
    this.typedSent = false;
    this.typedSinceChoice = true;
    this.editing = true;
    this.discarded.delete(value.trim());
    return this.session.type(value);
  }

  /**
   * I7, judged as the reader ends the edit or a hide sends it, before the hook hears of it: the
   * hook's draft, which it sends, is what the field holds by the reader's own record.
   */
  private checkField(): void {
    const shown = this.shown();
    const draft = this.session.drafts.title.trim();
    if (draft === shown) return;
    const read = `the field holds ${JSON.stringify(draft)} where the reader left ${JSON.stringify(shown)}`;
    this.noted.push({ invariant: "I7", read });
  }

  /**
   * The reader ends the edit, or a hide sends it: their choice is what the field holds by their
   * own record, when that is a title other than the one it is judged against - the title of the
   * write out while one is, since the issue is about to hold it, else the issue's. An end that
   * chooses nothing leaves the field on that title, as an escape does, unless the field is empty:
   * the hook refuses an empty title and leaves the field as it is.
   */
  private choose(): void {
    const { session } = this;
    const shown = this.shown();
    const against = (session.out?.title ?? session.issue.title).trim();
    this.chosen = shown !== "" && shown !== against ? shown : null;
    this.owed = this.chosen;
    this.stopped = false;
    this.typedSinceChoice = false;
    if (this.chosen === null && shown !== "") this.putBack();
  }

  /**
   * The field put back to the title it is judged against. While a title write is out that is the
   * title the write carries: the reader chose it when they ended the edit that sent it, so it is
   * their choice again, and a refusal of that write leaves it in the field as the choice not yet
   * sent. With nothing out it is the issue's title, which is no choice of theirs.
   */
  private putBack(): void {
    this.lastTyped = this.session.out?.title ?? null;
    this.typedSent = this.lastTyped !== null;
  }

  private judge(input: IssueUpdateInput): Finding[] {
    const { title } = input;
    if (title === undefined) {
      this.titleWriteOut = null;
      return [];
    }
    const findings: Finding[] = [];
    const quoted = JSON.stringify(title);
    // I1 holds a title sent to the reader's last ended choice (decision 324), not to the field's
    // text or to what the reader typed last: a queued title goes as the write ahead of it lands,
    // while the reader may be typing something else in an editor opened again.
    if (title !== this.chosen) {
      const choice = this.chosen === null ? "none" : JSON.stringify(this.chosen);
      const read = `sent ${quoted} while the reader's last ended choice was ${choice}`;
      findings.push({ invariant: "I1", read });
    }
    if (this.discarded.has(title)) {
      findings.push({ invariant: "I2", read: `sent ${quoted}, which an escape had put away` });
    }
    // A title sent again is the reader's choice again once the issue has moved off it, as another
    // reader can move it: only a title still the one it is judged against goes twice for nothing.
    // Writes go one at a time, so at a send nothing is out and that is the issue's title; the
    // title out is read only in case that ever changes.
    const previous = this.lastTitleWrite;
    const against = this.titleWriteOut?.title ?? this.session.issue.title.trim();
    if (previous !== null && previous.title === title && !previous.refused && against === title) {
      findings.push({ invariant: "I3", read: `sent ${quoted} twice in a row` });
    }
    this.chosen = null;
    if (title === this.owed) this.owed = null;
    if (title === this.lastTyped?.trim()) this.typedSent = true;
    const write = { title, refused: false };
    this.lastTitleWrite = write;
    this.titleWriteOut = write;
    return findings;
  }

  /**
   * I6 and I5, judged once an event is over. I6 holds the open editor to the text the reader
   * last typed in it; once the issue has made that text its own (`lastTyped` cleared), there is
   * no typed text of theirs left for it to hold. I5 holds an owed choice to being sent, as the
   * line allows, and to being held by the hook meanwhile, queued or as the field's text.
   */
  private afterEvent(event: EventName): Finding[] {
    const { session } = this;
    const findings: Finding[] = [];
    const draft = session.drafts.title.trim();
    const typed = this.lastTyped?.trim() ?? null;
    if (this.editing && typed !== null && answers[event] && draft !== typed) {
      const read = `the open editor holds ${JSON.stringify(draft)} where the reader typed ${JSON.stringify(typed)}`;
      findings.push({ invariant: "I6", read });
    }
    const against = (session.out?.title ?? session.issue.title).trim();
    if (this.owed === against) {
      if (session.out === null && typed === against && !this.typedSinceChoice) {
        this.lastTyped = null;
      }
      this.owed = null;
    }
    if (this.owed === null) return findings;
    const owed = JSON.stringify(this.owed);
    if (!session.drafts.titleQueued && draft !== this.owed) {
      findings.push({ invariant: "I5", read: `dropped ${owed}, a choice not yet sent` });
    } else if (session.out === null && !this.stopped) {
      findings.push({ invariant: "I5", read: `${owed}, a choice not yet sent, waits on nothing` });
    }
    return findings;
  }
}

interface Violation extends Finding {
  sequence: string;
  length: number;
}

/** One line of the trace: a sequence, every title it sent, and the title the field ended on. */
interface TraceLine {
  sequence: string;
  sent: string[];
  title: string;
  violates: Invariant[];
}

/**
 * I4, judged wherever no write is out: the field shows the issue's title or the reader's own
 * choice. A field that shows neither, a stale title the reader never typed, is what the next end
 * would send over the issue's title, so an end that would send it does not excuse it.
 */
function atRest(reader: Reader): Finding[] {
  const { session } = reader;
  const title = session.drafts.title.trim();
  const server = session.issue.title.trim();
  const chosen = reader.lastTyped?.trim() ?? null;
  if (title === server || title === chosen) return [];
  const typed = chosen === null ? "nothing" : JSON.stringify(chosen);
  return [
    {
      invariant: "I4",
      read: `at rest the field shows ${JSON.stringify(title)}, the issue holds ${JSON.stringify(server)}, the reader chose ${typed}`,
    },
  ];
}

const longest = 5;

/**
 * Plays every sequence of one to `longest` events. An event that does not apply where it falls (a
 * landing with nothing out) is never chosen there, since the sequence it would leave is a shorter
 * one already played. A hook cannot be copied, so each sequence is played from a fresh mount; one
 * play of a five-event sequence also stands for each of its prefixes, and each prefix is judged on
 * the play that reaches it first.
 */
function explore(): Violation[] {
  const violations: Violation[] = [];
  const trace = new Map<string, TraceLine>();
  // The choice at each depth of the sequence being played: the events that applied there, and
  // which one was taken. A depth past the last one changed is played for the first time.
  const path: { options: EventName[]; index: number }[] = [];
  let firstNew = 0;
  for (;;) {
    const reader = new Reader();
    const { session } = reader;
    const events: EventName[] = [];
    try {
      for (let depth = 0; depth < longest; depth += 1) {
        if (depth === path.length) {
          path.push({ options: alphabet.filter((event) => reader.applies(event)), index: 0 });
        }
        const choice = path[depth];
        const event = choice.options[choice.index];
        events.push(event);
        const sequence = events.join(" ");
        const findings = reader.play(event);
        if (depth < firstNew) continue;
        const line: TraceLine = {
          sequence,
          sent: session.titlesSent(),
          title: session.drafts.title,
          violates: [],
        };
        trace.set(sequence, line);
        const atRestFindings = session.out === null ? atRest(reader) : [];
        for (const finding of [...findings, ...atRestFindings]) {
          violations.push({ ...finding, sequence, length: events.length });
          line.violates.push(finding.invariant);
        }
      }
    } finally {
      cleanup();
    }
    // The next sequence takes the next event at the deepest choice that has one left.
    let last = path.at(-1);
    while (last !== undefined && last.index + 1 >= last.options.length) {
      path.pop();
      last = path.at(-1);
    }
    if (last === undefined) break;
    last.index += 1;
    firstNew = path.length - 1;
  }

  // Off by default: a trace of every sequence, one JSON line each, so two heads of the hook can be
  // compared sequence by sequence.
  const tracePath = process.env.DRAFTS_TRACE;
  if (tracePath) {
    const lines = [...trace.values()].map((line) => `${JSON.stringify(line)}\n`);
    writeFileSync(tracePath, lines.join(""));
  }
  violations.sort((a, b) => a.length - b.length);
  return violations;
}

// The seven tests below share one exploration: it is the slow part, and it judges all seven at
// once. It plays about 88,000 sequences of five events, which judge about 97,000 sequences of one
// to five, near a minute and a half on one core, so each of the seven has a time limit of its own
// above the suite's minute: whichever runs first pays for the exploration.
let explored: Violation[] | undefined;
function exploration(): Violation[] {
  explored ??= explore();
  return explored;
}
const explorationTimeout = 300_000;

// I2 and I3 each name a send that I1 also refuses, or one with nothing left to send. A value an
// escape put away is not the reader's last ended choice: the escape withdrew that choice, and only
// a later end chooses it again, while typing it again takes it out of what was put away. A value
// sent again while the title it is judged against - the title out, else the issue's - is still
// that value is not either, since an end chooses no title the field is judged against, or else it
// was chosen before the issue came to hold it, which left it nothing to send. I3 excuses a value
// sent again after a refusal, and one sent again once the issue's title, or the title out, had
// moved off it, as another reader can move it: the reader can choose it again then (decision
// 332). I2 and I3 say which of the sends I1 refuses was a value put away, and which a value sent
// twice.
const invariants: [Invariant, string][] = [
  ["I1", "nothing is sent but the reader's last ended choice"],
  ["I2", "a discarded value is never sent later"],
  [
    "I3",
    "no value is sent twice in a row, unless the first was refused or the issue's title, or the title out, had moved off it",
  ],
  ["I4", "at rest the field shows the server's value or the reader's unsent choice"],
  ["I5", "a choice the reader ended is sent"],
  ["I6", "text the reader typed in an open editor is never replaced by a value they did not type"],
  ["I7", "at the end of an edit the field holds what the reader left in it"],
];

for (const [invariant, statement] of invariants) {
  test(
    `every sequence of one to five events keeps ${invariant}: ${statement}`,
    () => {
      const broken = exploration().filter((v) => v.invariant === invariant);
      expect({
        sequences: new Set(broken.map((v) => v.sequence)).size,
        shortest: broken.slice(0, 5).map((v) => `${v.sequence}: ${v.invariant} ${v.read}`),
      }).toEqual({ sequences: 0, shortest: [] });
    },
    explorationTimeout
  );
}

// The three orders below were found in review, each a way the hook sent or showed a title the
// reader had not chosen.

test("an escape while a saved title is out, then an end with nothing typed, sends no second title", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.escape();
  session.end();
  session.land();
  expect(session.titlesSent()).toEqual(["a"]);
});

test("a draft typed back to the title out behind a queued save lets that save go as the write lands, and the end of the edit sends the draft", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type("ab");
  session.end();
  session.type("a");
  session.land();
  expect({ title: session.drafts.title, sent: session.titlesSent() }).toEqual({
    title: "a",
    sent: ["a", "ab"],
  });
  session.land();
  session.end();
  expect(session.titlesSent()).toEqual(["a", "ab", "a"]);
});

test("the issue's title typed back, queued and refused gives way to another reader's title and a route save sends only the route", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type(startIssue.title);
  session.end();
  session.refuse();
  session.otherReader("Plan the review");
  const title = session.drafts.title;
  const before = session.received.length;
  session.step(() => session.drafts.writeRoute("role:a"));
  session.step(() => session.drafts.requestRouteSubmit());
  expect({ title, routeSave: session.received.slice(before) }).toEqual({
    title: "Plan the review",
    routeSave: [{ route: "role:a" }],
  });
});

// A write that lands with no answer is passed the issue as it was (`next ?? issue` in both
// headers), which still holds the old title until the page reads the issue again. The field the
// write carried takes the value it carried, so the draft stays the title it sent and reads as not
// changed, rather than going back to the old title until that read lands.
test("a title write that lands with no answer keeps the title it sent", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.step(() => {
    session.drafts.onIssueSuccess(session.issue, { title: "a" });
    session.drafts.onIssueSettled(null);
  });
  expect({
    title: session.drafts.title,
    changed: session.drafts.titleDirty,
    sent: session.titlesSent(),
  }).toEqual({ title: "a", changed: false, sent: ["a"] });
});

// The order a sweep of the pages found (decision 324): a title saved behind the write out, and the
// editor opened again to type on with no end. The landing sends the saved title, the reader's
// last ended choice, and the field keeps what the reader is still typing. That title goes at the
// end of the edit, once the saved title's own write has landed.
test("a title typed on behind a queued save is not sent as the write ahead lands, and the end of the edit sends it", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type("ab");
  session.end();
  // One character typed onto the end, with no end of the edit.
  session.type("abc");
  session.land();
  const landed = { title: session.drafts.title, sent: session.titlesSent() };
  session.land();
  session.end();
  expect({ landed, ended: session.titlesSent() }).toEqual({
    landed: { title: "abc", sent: ["a", "ab"] },
    ended: ["a", "ab", "abc"],
  });
});

// Typing ends no edit, so the issue's own title typed back behind a queued save, with the edit
// still open, neither chooses it nor withdraws the save (decision 324): the save goes as the write
// ahead of it lands, and the issue's title goes once the reader ends the edit on it.
test("the issue's title typed back behind a queued save, with no end, lets the save go as the write ahead lands, and the end of the edit sends it", () => {
  const session = new Session();
  session.type("a");
  session.end();
  session.type("ab");
  session.end();
  session.type(startIssue.title);
  session.land();
  expect({ title: session.drafts.title, sent: session.titlesSent() }).toEqual({
    title: startIssue.title,
    sent: ["a", "ab"],
  });
  session.land();
  session.end();
  expect(session.titlesSent()).toEqual(["a", "ab", startIssue.title]);
});

// A title saved while a status write is out waits behind it, one write of the issue's fields at a
// time. Another reader sets the issue to that very title meanwhile, so it has nothing left to
// send: sent as the status write lands, it would be a second write of a title the issue holds.
// The reader's ended choice is then the issue's title, with nothing out and nothing owed, so the
// field is the issue's again and follows the next title another reader sets (decision 332): left
// the reader's, it held a title the issue had moved off, which the next end would send.
test("a title queued behind another field's write is not sent once another reader sets the issue to it, and the field then follows the issue's title", () => {
  const session = new Session();
  session.step(() => session.drafts.requestStatusSubmit("done"));
  session.type("X");
  session.end();
  session.otherReader("X");
  const landed = { ...session.issue, status: "done" };
  session.step(() => {
    session.drafts.onIssueSuccess(landed, { status: "done" });
    session.drafts.onIssueSettled(null);
  });
  session.otherReader("Plan the review");
  expect({ title: session.drafts.title, sent: session.received }).toEqual({
    title: "Plan the review",
    sent: [{ status: "done" }],
  });
});

// A title saved behind a status write waits there while the reader types on, and another reader
// sets the issue to the reader's text and then to a title of their own. The draft stays the
// reader's text: moved on to the other reader's title, it showed a title the reader never typed,
// and the end of the edit would have sent it over the issue's.
test("a draft with a title queued behind another field's write keeps the reader's text as another reader's titles arrive", () => {
  const session = new Session();
  session.step(() => session.drafts.requestStatusSubmit("done"));
  session.type("t1");
  session.end();
  session.type("t2");
  session.otherReader("t2");
  session.otherReader("o1");
  const shown = session.drafts.title;
  session.land();
  session.land();
  expect({ shown, title: session.drafts.title, sent: session.titlesSent() }).toEqual({
    shown: "t2",
    title: "t2",
    sent: ["t1"],
  });
});
