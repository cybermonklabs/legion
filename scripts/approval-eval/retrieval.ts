import type { CaseRow } from "./build-corpus";
import { artifactsOf, listOf, recordOf, stringOf } from "./corpus-io";
import { type DispatchConfig, dispatchConfig, readJson } from "./dispatch-read";
import { evidenceFor } from "./split";

/**
 * The nearest past decisions of a case, by meaning, for the many-shot arms.
 *
 * The route is Dispatch's own search (`GET /api/v1/search`), whose one ranked list mixes a
 * keyword leg with a meaning leg. A hit is kept only when it names an ask the corpus holds as a
 * case the dating rule allows: answered strictly earlier than the judged case and on the
 * training side of the cut (`evidenceFor`). Measured against this server over ten post-cut
 * approval cases, one page of 50 results holds between 2 and 8 such cases, so the search is
 * paged until the wanted count is reached or `SEARCH_PAGES` pages have been read: at four pages
 * six of those ten reached eight neighbours, and two of the remaining four reach eight by the
 * seventh page. A case still short of the count is answered with what the search found, and the
 * recipe's record carries that count.
 *
 * A neighbour on the judged case's own document or issue is dropped, however near it is: the
 * nearest neighbour of "Approve spec.md (version 7)?" is that document's own earlier round with
 * the person's verdict attached, which is a near-label given to two arms and withheld from two.
 * The answer carries how many were dropped for that, which the round records per case.
 *
 * Where the search stamps its answer `degraded` - its embedder is unavailable, so the meaning
 * leg did not run - or where it answers nothing usable at all, the fallback is a local lexical
 * ranker over the training slice: token overlap weighted by inverse document frequency. The
 * answer then says `route: "lexical"`, because the plan reports a lexical arm as lexical and
 * never as "by meaning".
 */

/** How many results one page of the search asks for, which is that route's own maximum. */
const SEARCH_PAGE = 50;

/** How many pages are read before the search is taken for as near as it will get. */
const SEARCH_PAGES = 8;

/** How long a query the search accepts: a longer one is refused 400 CAP_EXCEEDED. */
const QUERY_CHARS = 1000;

/** The shortest token the lexical ranker counts, below which a word carries no meaning. */
const TOKEN_CHARS = 3;

/** Which index answered: Dispatch's search by meaning, or the local lexical fallback. */
export type RetrievalRoute = "search" | "lexical";

/** How `nearestCases` is told what to look for. */
export interface NearestOptions {
  /** How many neighbours are wanted. */
  count: number;
  /** The cut, past which no case may be evidence. */
  cut: string | Date;
  /** False drops the time test: the negative control of `split.ts`, and never a recipe. */
  datingRule?: boolean;
  /** False keeps neighbours on the judged case's own document or issue. Defaults to true. */
  excludeSameDocument?: boolean;
  /** The server and the bearer, read from the environment when the caller hands none. */
  config?: DispatchConfig;
}

/** One neighbour, and where in the ranking it stood. */
export interface Neighbour {
  row: CaseRow;
  /** Its place in the ranking the route gave, counting from 1. */
  place: number;
  /** The score the route gave it: the search's own rank, or the lexical ranker's. */
  score: number;
}

/** What one retrieval found, and how. */
export interface NearestAnswer {
  neighbours: Neighbour[];
  route: RetrievalRoute;
  /** How many near cases were dropped for being on the judged case's own document or issue. */
  excluded: number;
  /** The `degraded` stamp the search answered, where it answered one. */
  degraded: string | null;
  /** How many cases the dating rule allowed at all, which is what was ranked over. */
  allowed: number;
  /** How many pages of the search were read. Zero on the lexical route. */
  pages: number;
}

/** One hit of `GET /api/v1/search`, of the fields this reads. */
interface SearchHit {
  kind: string;
  id: string;
  rank: number;
}

/** One answer of `GET /api/v1/search`, of the fields this reads. */
interface SearchAnswer {
  hits: SearchHit[];
  degraded: string | null;
  reachable: number;
}

/** Whether `other` sits on the judged case's own document or its own issue. */
function sharesGround(
  judged: CaseRow,
  judgedArtifacts: ReadonlySet<string>,
  other: CaseRow
): boolean {
  if (judged.issueKey !== null && other.issueKey === judged.issueKey) return true;
  for (const artifact of artifactsOf(other)) {
    if (judgedArtifacts.has(artifact)) return true;
  }
  return false;
}

async function searchPage(
  query: string,
  offset: number,
  config: DispatchConfig
): Promise<SearchAnswer> {
  const path = `/api/v1/search?q=${encodeURIComponent(query)}&limit=${SEARCH_PAGE}&offset=${offset}`;
  const answer = recordOf(await readJson<unknown>(path, config));
  const hits: SearchHit[] = [];
  for (const value of listOf(answer?.results)) {
    const hit = recordOf(value);
    const kind = stringOf(hit?.kind);
    const id = stringOf(hit?.id);
    if (kind === null || id === null) continue;
    hits.push({ kind, id, rank: typeof hit?.rank === "number" ? hit.rank : 0 });
  }
  return {
    hits,
    degraded: stringOf(answer?.degraded),
    reachable: typeof answer?.reachable === "number" ? answer.reachable : 0,
  };
}

/** The words of one case, as the lexical ranker reads them. */
function tokensOf(row: CaseRow): string[] {
  const text = [row.question ?? "", ...row.options.map((option) => option.label ?? "")].join(" ");
  return (text.toLowerCase().match(/[a-z0-9][a-z0-9_-]*/g) ?? []).filter(
    (token) => token.length >= TOKEN_CHARS
  );
}

/**
 * The lexical fallback: every allowed case scored by the inverse document frequency of the
 * query's words it carries, over the allowed cases themselves, and divided by the square root of
 * its own length so a long case does not win on length alone. Ties break on the case id, so the
 * ranking is the same on every run.
 */
function lexicalRanking(allowed: readonly CaseRow[], caseRow: CaseRow): Neighbour[] {
  const documents = allowed.map((row) => ({ row, tokens: new Set(tokensOf(row)) }));
  const frequency = new Map<string, number>();
  for (const document of documents) {
    for (const token of document.tokens) {
      frequency.set(token, (frequency.get(token) ?? 0) + 1);
    }
  }
  const query = new Set(tokensOf(caseRow));
  const scored = documents.map((document) => {
    let score = 0;
    for (const token of query) {
      if (!document.tokens.has(token)) continue;
      score += Math.log(documents.length / (1 + (frequency.get(token) ?? 0)));
    }
    return { row: document.row, score: score / Math.sqrt(Math.max(1, document.tokens.size)) };
  });
  scored.sort((one, other) =>
    other.score === one.score
      ? one.row.caseId < other.row.caseId
        ? -1
        : 1
      : other.score - one.score
  );
  return scored.map((one, index) => ({ row: one.row, place: index + 1, score: one.score }));
}

/**
 * The nearest `count` cases the dating rule allows, by meaning, with the route that found them.
 * Neighbours on the judged case's own document or issue are dropped and counted.
 */
export async function nearestCases(
  corpus: readonly CaseRow[],
  caseRow: CaseRow,
  options: NearestOptions
): Promise<NearestAnswer> {
  const allowed = evidenceFor(corpus, caseRow, {
    cut: options.cut,
    datingRule: options.datingRule,
  });
  const byAskId = new Map<string, CaseRow>();
  for (const row of allowed) byAskId.set(row.askId, row);
  const judgedArtifacts = artifactsOf(caseRow);
  const apart = options.excludeSameDocument !== false;
  const query = (caseRow.question ?? "").replace(/\s+/g, " ").trim().slice(0, QUERY_CHARS);
  const neighbours: Neighbour[] = [];
  const seen = new Set<string>();
  let excluded = 0;
  let degraded: string | null = null;
  let pages = 0;
  if (query !== "" && allowed.length > 0) {
    const config = options.config ?? (await dispatchConfig());
    for (let page = 0; page < SEARCH_PAGES && neighbours.length < options.count; page += 1) {
      const answer = await searchPage(query, page * SEARCH_PAGE, config);
      pages += 1;
      degraded = answer.degraded;
      for (const hit of answer.hits) {
        if (hit.kind !== "ask") continue;
        const row = byAskId.get(hit.id);
        if (row === undefined || seen.has(row.caseId)) continue;
        seen.add(row.caseId);
        if (apart && sharesGround(caseRow, judgedArtifacts, row)) {
          excluded += 1;
          continue;
        }
        if (neighbours.length >= options.count) break;
        neighbours.push({ row, place: neighbours.length + 1, score: hit.rank });
      }
      if (degraded !== null && degraded !== "") break;
      if (answer.hits.length < SEARCH_PAGE) break;
      if ((page + 1) * SEARCH_PAGE >= answer.reachable) break;
    }
  }
  const stood = degraded !== null && degraded !== "";
  if (!stood && neighbours.length > 0) {
    return { neighbours, route: "search", excluded, degraded, allowed: allowed.length, pages };
  }
  // Degraded, or nothing the corpus holds: the local ranker, named as what it is.
  const lexical: Neighbour[] = [];
  let lexicalExcluded = 0;
  for (const neighbour of lexicalRanking(allowed, caseRow)) {
    if (apart && sharesGround(caseRow, judgedArtifacts, neighbour.row)) {
      lexicalExcluded += 1;
      continue;
    }
    if (lexical.length >= options.count) break;
    lexical.push({ ...neighbour, place: lexical.length + 1 });
  }
  return {
    neighbours: lexical,
    route: "lexical",
    excluded: lexicalExcluded,
    degraded,
    allowed: allowed.length,
    pages,
  };
}
