import { expect, test } from "bun:test";
import { closeSync, openSync, writeSync } from "node:fs";
import { setImmediate as nextTurn } from "node:timers/promises";
import { cleanup } from "@testing-library/react";

import { Session } from "./issue-drafts-fixture";
import type { IssueUpdateInput } from "./useIssueDrafts";

// The title draft of both issue headers is one hook, and the bugs found in it have each been one
// particular order of typing, saving, escaping and answers from Dispatch. Rather than wait for the
// next order to be found by hand, this file plays every order up to five events long and checks,
// after every event, the seven things decisions 316, 324 and 332 say a reader can rely on. The
// orders a review or a sweep of the pages found are useIssueDrafts.test.ts's, each as its own
// test, so a fix can be read against them by name.

const alphabet = [
  "typeNew",
  "typeServer",
  "typeOut",
  "typeMore",
  "typeSpace",
  "saveNew",
  "end",
  "hide",
  "escape",
  "land",
  "refuse",
  "otherReader",
  "otherShown",
  "otherOwed",
  "otherOut",
  "otherFieldLands",
] as const;
type EventName = (typeof alphabet)[number];
/** The events by which another reader sets the issue to a title of the reader's own. */
type OwnTitleEvent = "otherShown" | "otherOwed" | "otherOut";
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
  otherShown: true,
  otherOwed: true,
  otherOut: true,
  otherFieldLands: true,
};

/**
 * A session and a small model of its reader: what the field holds by their own record, what they
 * last chose and still wait to see sent, what they put away, and what has been sent, which is
 * what the invariants are judged against. The model never takes a choice from the hook's draft:
 * a hook that put a wrong value in the draft would have it counted as the reader's choice. Whose
 * the field is, the reader's or the issue's, is taken from decision 332's words, not from the
 * hook's rules. A keystroke decides it. Of the answers (a landing, a refusal, another reader's
 * title), only the settle of the title's own write hands a typed or ended field back to the
 * issue, never another reader's title; with nothing out, the reader's own keystroke back to the
 * issue's title with nothing owed, an end that chose nothing and an escape do too.
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
   * The reader's own text the field shows by their record, as typed, or none while the field is
   * the issue's and follows the issue's title (decision 332). A type event sets it to its text,
   * unless that text, as typed, is the title it is judged against - the title out while one is,
   * else the issue's - with nothing owed: that hands the field back, to the issue with nothing
   * out, and with a write out to the title out, the reader's ended choice, as an escape or an end
   * that chose nothing puts it back (`putBack`). An end or a hide that chooses a title sets it to
   * that title trimmed, since every end trims what it sends (`choose`). It is none once the title
   * write's own landing or refusal leaves the field the issue's (`settle`). Another reader's
   * title never clears it, not even one equal to it: no keystroke judged that title.
   */
  lastTyped: string | null = null;
  /**
   * Whether `lastTyped` is text I6 holds the open editor to: the reader typed it, it differed, as
   * typed, from the title it was judged against at the keystroke that left it, or was typed while
   * a choice was owed, and no write has landed with it since.
   */
  private marked = false;
  /**
   * Whether `lastTyped` is the reader's ended choice: the title an end or a hide chose, or the
   * title out that a keystroke back to it with nothing owed, an escape or an end that chose
   * nothing puts back while that write is out. A keystroke that marks the text clears it.
   */
  private ended = false;
  /** The drafts an escape put away, until the reader types one of them again. */
  private readonly discarded = new Set<string>();
  private lastTitleWrite: TitleWrite | null = null;
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
      case "otherShown":
      case "otherOwed":
      case "otherOut":
        return this.ownTitles()[event] !== undefined;
      default:
        return true;
    }
  }

  /**
   * The titles of the reader's own that another reader can set the issue to: the title the field
   * shows by the reader's record, their owed choice, and the title out. Each is there only where
   * it is a title other than the issue's, which would change nothing, and other than the one an
   * event before it in this list sets, which would play that event's branch again.
   */
  private ownTitles(): Partial<Record<OwnTitleEvent, string>> {
    const { session } = this;
    const titles: Partial<Record<OwnTitleEvent, string>> = {};
    const taken = new Set([session.issue.title.trim()]);
    const candidates: [OwnTitleEvent, string | null][] = [
      ["otherShown", this.shown()],
      ["otherOwed", this.owed],
      ["otherOut", session.out?.title ?? null],
    ];
    for (const [event, title] of candidates) {
      if (title === null || taken.has(title)) continue;
      taken.add(title);
      titles[event] = title;
    }
    return titles;
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

  /** What the field holds by the reader's own record, as typed: the text they last typed while it
   *  is theirs, else the title of the write out, else the issue's title. */
  private held(): string {
    const { session } = this;
    return this.lastTyped ?? session.out?.title ?? session.issue.title;
  }

  /** What the field holds by the reader's own record, trimmed, as an end would send it. */
  private shown(): string {
    return this.held().trim();
  }

  /** The title the field is judged against, as the hook's `reference` is: the title of the write
   *  out while one is, since the issue is about to hold it, else the issue's title. */
  private against(): string {
    const { session } = this;
    return (session.out?.title ?? session.issue.title).trim();
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
        // One character onto the end of what the field holds by the reader's record, trimmed
        // (`shown`), as a reader goes on writing without ending the edit.
        return this.type(`${this.shown()}x`);
      case "typeSpace":
        // One space onto the end of what the field holds by the reader's record, trimmed
        // (`shown`), as a reader types before the next word: the text as typed is theirs, though
        // an end sends it trimmed.
        return this.type(`${this.shown()} `);
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
        if (shown !== "" && shown !== this.against()) this.choose();
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
        this.stopped = false;
        const landed = session.out?.title;
        // The write landed with the field's text when that text, as typed, is the title it
        // carried: a space typed after it while the write was out is text the write never held.
        if (landed !== undefined) this.settle(landed, this.lastTyped === landed);
        return session.land();
      }
      case "refuse":
        this.stopped = true;
        if (session.out?.title !== undefined) {
          // The write out is the last write sent, so a title write out is the last title write.
          if (this.lastTitleWrite !== null) this.lastTitleWrite.refused = true;
          this.settle(session.issue.title.trim(), false);
        }
        return session.refuse();
      case "otherReader":
        this.others += 1;
        return session.otherReader(`o${this.others}`);
      case "otherShown":
      case "otherOwed":
      case "otherOut": {
        const title = this.ownTitles()[event];
        if (title === undefined) throw new Error(`${event} played where it does not apply`);
        return session.otherReader(title);
      }
      case "otherFieldLands":
        this.picks += 1;
        return session.otherFieldLands(`p${this.picks}`);
    }
  }

  /**
   * A keystroke (decision 332). One that leaves the field's text, as typed, on the title it is
   * judged against - the title out while one is, else the issue's - with nothing owed hands the
   * field back: with nothing out it is the issue's, and with a write out it is the title out, the
   * reader's ended choice. Any other keystroke makes the field the reader's typed text, the change
   * mark. A space typed before the next word is the reader's text, though an end sends it trimmed,
   * so it is compared as typed: handed back, the field took a rename that arrived before the next
   * keystroke. With a choice owed the field does not follow the issue (decision 324), so even a
   * keystroke back to the title it is judged against leaves it the reader's.
   */
  private type(value: string): IssueUpdateInput[] {
    const { session } = this;
    if (this.owed === null && value === this.against()) {
      this.putBack();
    } else {
      this.lastTyped = value;
      this.marked = true;
      this.ended = false;
    }
    this.editing = true;
    this.discarded.delete(value.trim());
    return session.type(value);
  }

  /**
   * The title write out lands or is refused, and the issue's title is then `title` (decision
   * 332). An owed choice the issue's title has become has nothing left to send (decision 324). A
   * write that landed with the field's text, as typed, has seen it land, which I6 then no longer
   * holds. Of the answers (a landing, a refusal, another reader's title), this settle is the only
   * one that hands a typed or ended field back to the issue, never another reader's title; with
   * nothing out, the reader's own keystroke back to the issue's title with nothing owed, an end
   * that chose nothing and an escape do too (`putBack`). The settle hands it back when nothing is
   * owed, the field holds the issue's title, and the write landed with it or it was the reader's
   * ended choice, as an undo to the issue's own title is once a refusal of the write ahead of it
   * leaves the issue on that title. Whether the write landed with the field's text compares that
   * text as typed, as a keystroke does: a space typed before the next word while the write was
   * out is the reader's text, which neither the write nor the issue holds, so the field stays
   * theirs and keeps it.
   */
  private settle(title: string, landedWithIt: boolean): void {
    if (this.owed === title) this.owed = null;
    if (landedWithIt) this.marked = false;
    const handedBack = this.owed === null && this.shown() === title && (landedWithIt || this.ended);
    if (handedBack) {
      this.lastTyped = null;
      this.marked = false;
      this.ended = false;
    }
  }

  /**
   * I7, judged as the reader ends the edit or a hide sends it, before the hook hears of it: the
   * hook's draft is what the field holds by the reader's own record, as typed. The end trims what
   * it sends only once it is heard, so a draft that lost a space the reader typed before their
   * next word had changed under them, even though it sends the same title.
   */
  private checkField(): void {
    const left = this.held();
    const draft = this.session.drafts.title;
    if (draft === left) return;
    const read = `the field holds ${JSON.stringify(draft)} where the reader left ${JSON.stringify(left)}`;
    this.noted.push({ invariant: "I7", read });
  }

  /**
   * The reader ends the edit, or a hide sends it: their choice is what the field holds by their
   * own record, trimmed, when that is a title other than the one it is judged against - the title
   * of the write out while one is, since the issue is about to hold it, else the issue's. Every
   * end trims, so the field then holds that choice trimmed, the title its write lands with, and
   * that write's settle hands the field back to the issue if the field still holds that title and
   * nothing is owed then (`settle`). An end that chooses nothing leaves the field on that title,
   * as an escape does, unless the field is empty: the hook refuses an empty title and leaves the
   * field as it is.
   */
  private choose(): void {
    const shown = this.shown();
    this.chosen = shown !== "" && shown !== this.against() ? shown : null;
    this.owed = this.chosen;
    this.stopped = false;
    if (this.chosen !== null) {
      this.lastTyped = this.chosen;
      this.ended = true;
    } else if (shown !== "") {
      this.putBack();
    }
  }

  /**
   * The field put back to the title it is judged against, by an escape, an end that chose
   * nothing, or a keystroke back to it with nothing owed. While a title write is out that is the
   * title the write carries: the reader chose it when they ended the edit that sent it, so it is
   * their ended choice again, and a refusal of that write leaves it in the field as the choice not
   * yet sent. With nothing out it is the issue's title, which is no choice of theirs.
   */
  private putBack(): void {
    this.lastTyped = this.session.out?.title ?? null;
    this.marked = false;
    this.ended = this.lastTyped !== null;
  }

  private judge(input: IssueUpdateInput): Finding[] {
    const { title } = input;
    if (title === undefined) return [];
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
    // reader can move it: only a title still the issue's goes twice for nothing.
    const previous = this.lastTitleWrite;
    const against = this.session.issue.title.trim();
    if (previous !== null && previous.title === title && !previous.refused && against === title) {
      findings.push({ invariant: "I3", read: `sent ${quoted} twice in a row` });
    }
    this.chosen = null;
    if (title === this.owed) this.owed = null;
    this.lastTitleWrite = { title, refused: false };
    return findings;
  }

  /**
   * I6 and I5, judged once an event is over. I6 holds the open editor to the text the reader
   * typed in it that the change mark covers (`marked`), as typed: text that differed from the
   * title it was judged against at its last keystroke, or was typed while a choice was owed, and
   * has not yet seen land. Once the field is the issue's (`lastTyped` cleared) there is no text of
   * theirs left for it to hold. I5 holds an owed choice to being sent, as the line allows, and to
   * being held by the hook meanwhile, queued or as the field's text. Another reader's title clears
   * no text of the reader's: it only leaves nothing to send of an owed choice it is the title of,
   * as the title the choice is judged against.
   */
  private afterEvent(event: EventName): Finding[] {
    const { session } = this;
    const findings: Finding[] = [];
    const draft = session.drafts.title;
    const typed = this.lastTyped;
    if (this.editing && this.marked && typed !== null && answers[event] && draft !== typed) {
      const read = `the open editor holds ${JSON.stringify(draft)} where the reader typed ${JSON.stringify(typed)}`;
      findings.push({ invariant: "I6", read });
    }
    if (this.owed === this.against()) this.owed = null;
    if (this.owed === null) return findings;
    const owed = JSON.stringify(this.owed);
    if (!session.drafts.titleQueued && draft.trim() !== this.owed) {
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

/** What the sweep keeps of one check: how many sequences broke it, and its shortest findings, in
 *  the order a sort of every finding by length that keeps their order would leave them. */
interface Broken {
  sequences: number;
  shortest: Violation[];
}

/** How many of a check's shortest findings its test prints. */
const printed = 5;

/** Keeps `violation` among the shortest findings of its check: after every finding no longer
 *  than it, which were found first, and only while it is one of the first `printed`. */
function keepShortest(shortest: Violation[], violation: Violation): void {
  let at = shortest.length;
  while (at > 0 && shortest[at - 1].length > violation.length) at -= 1;
  if (at === printed) return;
  shortest.splice(at, 0, violation);
  if (shortest.length > printed) shortest.pop();
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

/** The sequences played between two turns of the event loop: few enough that what they leave
 *  queued stays small, and enough that the turns cost little. A turn after every sequence made
 *  the sweep about a quarter slower. */
const sequencesPerTurn = 100;

/** What the sweep keeps: each check's count of sequences and shortest findings, and how many
 *  sequences it judged. */
interface Exploration {
  broken: Map<Invariant, Broken>;
  judged: number;
}

/**
 * Plays every sequence of one to `longest` events. An event that does not apply where it falls (a
 * landing with nothing out) is never chosen there, since the sequence it would leave is a shorter
 * one already played. A hook cannot be copied, so each sequence is played from a fresh mount; one
 * play of a five-event sequence also stands for each of its prefixes, and each prefix is judged on
 * the play that reaches it first.
 *
 * The sweep keeps nothing of the sequences it has judged but each check's count of sequences and
 * its shortest findings, and how many it judged, and writes a sequence's line of the trace to the
 * file as it is judged.
 * It lets each tree go: a sequence leaves microtasks queued that hold its unmounted tree until
 * they run, and React's development build puts about nine measures a sequence on the performance
 * timeline, which keeps each one until it is cleared. Played with no turn of the event loop and
 * the timeline kept, the sweep took over 3 GiB of a pod that every role of an issue shares.
 */
async function explore(): Promise<Exploration> {
  const broken = new Map<Invariant, Broken>();
  let judged = 0;
  // Off by default: a trace of every sequence, one JSON line each, so two heads of the hook can be
  // compared sequence by sequence.
  const tracePath = process.env.DRAFTS_TRACE;
  const trace = tracePath ? openSync(tracePath, "w") : null;
  // The choice at each depth of the sequence being played: the events that applied there, and
  // which one was taken. A depth past the last one changed is played for the first time.
  const path: { options: EventName[]; index: number }[] = [];
  let firstNew = 0;
  let played = 0;
  try {
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
          judged += 1;
          if (session.out === null) findings.push(...atRest(reader));
          if (trace !== null) {
            const line: TraceLine = {
              sequence,
              sent: session.titlesSent(),
              title: session.drafts.title,
              violates: findings.map((finding) => finding.invariant),
            };
            writeSync(trace, `${JSON.stringify(line)}\n`);
          }
          const brokenHere = new Set<Invariant>();
          for (const finding of findings) {
            let check = broken.get(finding.invariant);
            if (check === undefined) {
              check = { sequences: 0, shortest: [] };
              broken.set(finding.invariant, check);
            }
            if (!brokenHere.has(finding.invariant)) {
              brokenHere.add(finding.invariant);
              check.sequences += 1;
            }
            keepShortest(check.shortest, { ...finding, sequence, length: events.length });
          }
        }
      } finally {
        // The sequence's React tree goes before the next one mounts.
        cleanup();
      }
      played += 1;
      if (played % sequencesPerTurn === 0) {
        performance.clearMeasures();
        // The turn runs the microtasks these sequences left, which lets their trees go.
        await nextTurn();
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
  } finally {
    if (trace !== null) closeSync(trace);
  }
  return { broken, judged };
}

// The eight tests below - the seven checks and the count - share one exploration: it is the slow
// part, and it judges all seven checks at once. It plays about 171,000 sequences of five events,
// which judge 186,538 sequences of one to five, about 147 to 255 s in this pod on one core. So this
// file runs alone in package.json's `test:web:drafts`, whose --timeout holds the exploration, and
// the web lane, `test:web`, leaves it out: whichever of the eight runs first pays for it.
let explored: Promise<Exploration> | undefined;
function exploration(): Promise<Exploration> {
  explored ??= explore();
  return explored;
}

// I2 and I3 each name a send that I1 also refuses, or one with nothing left to send. A value an
// escape put away is not the reader's last ended choice: the escape withdrew that choice, and only
// a later end chooses it again, while typing it again takes it out of what was put away. A value
// sent again while the title it is judged against - the title out, else the issue's - is still
// that value is not either, since an end chooses no title the field is judged against, or else it
// was chosen before the issue came to hold it, which left it nothing to send. I3 excuses a value
// sent again after a refusal, and one sent again once the issue's title had moved off it, as
// another reader can move it: the reader can choose it again then (decision 332). I2 and I3 say
// which of the sends I1 refuses was a value put away, and which a value sent twice.
const invariants: [Invariant, string][] = [
  ["I1", "nothing is sent but the reader's last ended choice"],
  ["I2", "a discarded value is never sent later"],
  [
    "I3",
    "no value is sent twice in a row, unless the first was refused or the issue's title had moved off it",
  ],
  ["I4", "at rest the field shows the server's value or the reader's unsent choice"],
  ["I5", "a choice the reader ended is sent"],
  [
    "I6",
    "text the reader typed in an open editor, differing from the title it was judged against at its last keystroke or typed while a title was queued, and not yet seen land, is never replaced by a value they did not type",
  ],
  ["I7", "at the end of an edit the field holds what the reader left in it"],
];

for (const [invariant, statement] of invariants) {
  test(`every sequence of one to five events keeps ${invariant}: ${statement}`, async () => {
    const broken = (await exploration()).broken.get(invariant) ?? {
      sequences: 0,
      shortest: [],
    };
    expect({
      sequences: broken.sequences,
      shortest: broken.shortest.map((v) => `${v.sequence}: ${v.invariant} ${v.read}`),
    }).toEqual({ sequences: 0, shortest: [] });
  });
}

// How many sequences the seven checks judged: every sequence of one to five events the alphabet
// allows, each judged once. A sweep that stopped early, or judged a sequence twice, would pass the
// seven checks with this number wrong, so a change to the alphabet, to what an event applies to,
// or to the enumeration states its new count here.
test("the sweep judges every sequence of one to five events once: 186,538", async () => {
  expect((await exploration()).judged).toBe(186_538);
});
