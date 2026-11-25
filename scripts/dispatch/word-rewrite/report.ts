import { approvalSkip } from "./approval-skips.ts";
import { authorName, type Changes, versionLink } from "./changes.ts";
import { ownerName } from "./dispatch-api.ts";
import type { Examined, Item } from "./inventory.ts";
import type { PacedWrite } from "./pace.ts";
import { REASONS } from "./places.ts";
import type { RefusedOccurrence } from "./state.ts";
import { VERDICT_CODES, type VerdictCode, verdictName } from "./verdicts.ts";

/**
 * What a run prints: one line per occurrence, with the place that leaves it, `rewritten` for one
 * this run's write rewrote, the verdict a state holds for it, `judge-refused` with its item and
 * key, or `not-judged`; the counts by kind of item and project and by reason; the check blocks of
 * open issues that name an old name of the map; in report mode, the occurrences not judged that
 * an event after the last write-mode run's cursor changed (`changes.ts`); and, from the run, the
 * daemons, Dispatch's build, the pace and the size the state would reach. No line holds a text of
 * an item: keys, ids, slugs, offsets and fixed words alone.
 *
 * A place a program can tell comes first: a judge-refused occurrence in such a place is listed
 * under it while the place holds, and as judge-refused again once it ends. A judge-refused
 * occurrence counts as not judged (spec version 41).
 */

/** What a write did to an occurrence it judged, as the run that wrote it lists it: rewrote it, or
 *  left it with the verdict it saved under the new paragraph's key (`write.ts`). */
export type WrittenPlace = VerdictCode | "rewritten";

/** The counts and lines of one run, added to item by item. */
export class Tally {
  readonly lines: string[] = [];
  readonly checkLines: string[] = [];
  /** The occurrences not judged that an event after the last write-mode run's cursor changed. */
  readonly changedLines: string[] = [];
  readonly byKind = new Map<string, { occurrences: number; notJudged: number }>();
  readonly byReason = new Map<string, number>();
  readonly byVerdict = new Map<string, number>();
  /** The items that hold occurrences that wait for the judge: no place, verdict or code. */
  readonly waiting: Item[] = [];
  /** The writes the settle command makes for the judge-refused occurrences listed: one per item,
   *  by the item's owner and project, as the pace counts them. */
  readonly settles = new Map<string, PacedWrite>();
  /** The occurrences no place or verdict leaves, the judge-refused ones among them. */
  notJudged = 0;
  /** Occurrences whose two answers failed: for the tester's settle command. */
  judgeRefused = 0;
  /** Occurrences this run's writes rewrote. */
  rewritten = 0;
  items = 0;
  /** The check-name lines whose document a person approved at its latest version. */
  approvedLatestChecks = 0;
  /** The items this run examined. */
  readonly examined = new Set<string>();

  /** `changes`: the events after the last write-mode run's cursor, where the run lists them. */
  constructor(private readonly changes?: Changes) {}

  /** Adds one examined item; `verdicts` are the state's, and `written` what this run's write of
   *  the item did to each occurrence it judged, by its key as read (an item a run wrote is listed
   *  as it was read, and its verdicts stand under the new paragraph's keys). */
  add(
    examined: Examined,
    verdicts: ReadonlyMap<string, VerdictCode>,
    written?: ReadonlyMap<string, WrittenPlace>
  ): void {
    this.examined.add(examined.item.id);
    const item = examined.item;
    this.items += 1;
    const issue = item.issue?.key ?? "-";
    const document = item.artifact?.slug ?? "-";
    // A check block of a document a person approved at its latest version is marked: changing
    // the block would cost the person a new approval. The approval is the one the item was read
    // with, as the place `approved-by-person` reads it.
    const approvedLatest =
      item.artifact !== null &&
      approvalSkip({
        primary: item.artifact.primary === true,
        approval: item.artifact.approval,
        blocks: examined.blocks,
      }) === "approved-by-person";
    for (const check of examined.checkNames) {
      this.checkLines.push(
        `check-name issue=${issue} document=${document} block=${check.block} old=${check.old}${approvedLatest ? " approved=latest" : ""}`
      );
      if (approvedLatest) this.approvedLatestChecks += 1;
    }
    if (examined.occurrences.length === 0) return;
    const change = this.changes?.of(item);
    const kindKey = `${item.kind} ${item.project}`;
    const counts = this.byKind.get(kindKey) ?? { occurrences: 0, notJudged: 0 };
    let waits = false;
    for (const occurrence of examined.occurrences) {
      counts.occurrences += 1;
      let place: string;
      const done = occurrence.key === null ? undefined : written?.get(occurrence.key);
      const code =
        done === "rewritten"
          ? undefined
          : (done ?? (occurrence.key === null ? undefined : verdicts.get(occurrence.key)));
      if (occurrence.reason !== null) {
        place = `reason=${occurrence.reason}`;
        this.byReason.set(occurrence.reason, (this.byReason.get(occurrence.reason) ?? 0) + 1);
      } else if (done === "rewritten") {
        place = "rewritten";
        this.rewritten += 1;
      } else if (code === VERDICT_CODES["judge-refused"]) {
        // What the settle command's answer names it by: its key, and its offset in its paragraph
        // for a rewrite's `left`.
        const at = occurrence.start - (occurrence.paragraph?.start ?? 0);
        place = `judge-refused item=${item.id} key=${occurrence.key} at=${at}`;
        this.judgeRefused += 1;
        this.settles.set(item.id, { owner: ownerName(item.owner), project: item.project });
        counts.notJudged += 1;
        this.notJudged += 1;
      } else if (code !== undefined) {
        const name = verdictName(code) ?? code;
        place = `verdict=${name}`;
        this.byVerdict.set(name, (this.byVerdict.get(name) ?? 0) + 1);
      } else {
        place = "not-judged";
        counts.notJudged += 1;
        this.notJudged += 1;
        waits = true;
      }
      const at = occurrence.inName ? "name" : String(occurrence.byteStart);
      const name = occurrence.name === null ? "" : ` name=${occurrence.name.old}`;
      const where = `kind=${item.kind} project=${item.project} issue=${issue} document=${document} byte=${at}`;
      this.lines.push(`occurrence ${where} ${place}${name}`);
      const unjudged =
        occurrence.reason === null &&
        done !== "rewritten" &&
        (code === undefined || code === VERDICT_CODES["judge-refused"]);
      if (change !== undefined && unjudged) {
        const link = versionLink(item, change);
        this.changedLines.push(
          `changed-since ${where} event=${change.event} author=${authorName(change.author)}${link === null ? "" : ` version=${link}`}`
        );
      }
    }
    this.byKind.set(kindKey, counts);
    if (waits) this.waiting.push(item);
  }

  /** Lists, from the state, each occurrence kept as `judge-refused` whose item this run did not
   *  read (a stop time came first, or the item is gone): the state names its item and its key. */
  addKeptRefused(refused: readonly RefusedOccurrence[]): void {
    for (const occurrence of refused) {
      if (this.examined.has(occurrence.id)) continue;
      this.judgeRefused += 1;
      this.notJudged += 1;
      this.settles.set(occurrence.id, {
        owner: occurrence.issue ?? `${occurrence.project}/${occurrence.id}`,
        project: occurrence.project,
      });
      this.lines.push(
        `occurrence project=${occurrence.project} issue=${occurrence.issue ?? "-"} item=${occurrence.id} key=${occurrence.key} judge-refused from=state`
      );
    }
  }

  /** The counts, in a fixed order: kind and project, reason and verdict, then the occurrences
   *  rewritten, judge-refused and not judged, and the items read. */
  summary(): string[] {
    const lines = [...this.byKind.entries()]
      .sort(([a], [b]) => a.localeCompare(b))
      .map(
        ([key, counts]) =>
          `count ${key} occurrences=${counts.occurrences} not-judged=${counts.notJudged}`
      );
    for (const reason of REASONS) lines.push(`reason ${reason} ${this.byReason.get(reason) ?? 0}`);
    for (const [name, count] of [...this.byVerdict.entries()].sort()) {
      lines.push(`verdict ${name} ${count}`);
    }
    lines.push(`rewritten ${this.rewritten}`);
    lines.push(`judge-refused ${this.judgeRefused}`);
    lines.push(`not-judged ${this.notJudged}`);
    lines.push(`items-read ${this.items}`);
    return lines;
  }

  /** The report's section of check blocks that name an old name, counted, with how many of them
   *  stand in a document a person approved at its latest version. */
  checkSection(): string[] {
    return [
      ...this.checkLines,
      `check-names ${this.checkLines.length} approved-latest=${this.approvedLatestChecks}`,
    ];
  }

  /** The report's section of occurrences changed since the last write-mode run, counted: no
   *  failure, since the next write-mode run judges them. */
  changedSection(): string[] {
    return [...this.changedLines, `changed-since ${this.changedLines.length}`];
  }

  /** The owners of the items that wait for the judge, each once. */
  owners(): Item["owner"][] {
    const seen = new Map<string, Item["owner"]>();
    for (const item of this.waiting) seen.set(ownerName(item.owner), item.owner);
    return [...seen.values()];
  }
}
