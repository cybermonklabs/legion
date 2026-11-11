import type { CaseRow } from "./build-corpus";

/**
 * What the agent never decides, whatever its confidence.
 *
 * This is requirement Q7 of dispatch://INFRA-2897/spec as one rule: a document that asks for a
 * destructive act, for a credential, for a setting only an organization owner can make, for a
 * message to somebody outside the company, or for an account on an outside platform; a document
 * that changes these skills or their numbers; and the primary document of a parked root carrying
 * the `legion` label, because Dispatch records such an approval as the person's own and the
 * Legion daemon then admits that root (`personHandoverQueues` in
 * `packages/daemon/internal/workflow/gate.go`, where parked is the `backlog` or `icebox` status).
 *
 * The same rule serves two readers: the eval, which leaves every held case out of its slices
 * before the draw, and the skill the round ships, which refuses to decide a held request. One
 * rule, so the coverage the eval reports is the coverage the skill can deliver.
 *
 * It is a list of named kinds, each with the signals that raise it, read over the rendered
 * evidence text and the case row. It is meant to be conservative: holding a case it need not
 * hold costs the eval one case, and deciding one it must not hold costs a person something they
 * never agreed to. Each signal is therefore a phrase, not a lone common word, so that the rule
 * is readable and a reader can see what would fire it.
 *
 * Whether the issue is a parked root carrying the `legion` label is not in the rendered case, and
 * this rule reads no Dispatch: the caller hands that fact in as `issue`, so the rule stays one
 * pure function that a test can run over hand-written text. A caller with no such fact gets no
 * hold of that kind.
 */

/** The named kinds of request the agent never decides. */
export type NeverDecideKind =
  | "destructive-act"
  | "credential"
  | "owner-only-setting"
  | "outside-message"
  | "outside-account"
  | "changes-these-skills"
  | "parked-legion-root";

/** What the rule was told about the issue, which the rendered case does not carry. */
export interface IssueFacts {
  /** The parent issue's key, and null for a root. */
  parent: string | null;
  /** The issue's labels, among which `legion` makes it a Legion tree. */
  labels: readonly string[];
  /** The issue's status, of which `backlog` and `icebox` are parked. */
  status: string;
  /** The id of the issue's primary document, which an approval of admits the root. */
  primaryArtifactId: string | null;
}

/** One case, or one document, put to the rule. */
export interface NeverDecideInput {
  /** The evidence text as `renderCase` renders it. */
  text: string;
  /** The case being judged, where there is one: its documents say which artifact is asked about. */
  row?: CaseRow | null;
  /** The issue's own facts, which only the parked-root kind reads. */
  issue?: IssueFacts | null;
}

/** What the rule says about one case: whether it is held, under which kind, and on what signal. */
export interface NeverDecideAnswer {
  held: boolean;
  kind: NeverDecideKind | null;
  reason: string;
}

/** One signal of one kind: the phrase that raises it, named so a reason can say which fired. */
interface Signal {
  name: string;
  pattern: RegExp;
}

/** One kind the agent never decides, with what it is and the signals that raise it. */
export interface NeverDecideRule {
  kind: NeverDecideKind;
  /** What this kind is, in the words of Q7. */
  what: string;
  signals: readonly Signal[];
}

/** The statuses at which a root is parked, waiting for a person rather than running. */
const PARKED: readonly string[] = ["backlog", "icebox"];

/**
 * Who a document can direct an act at. A document of this corpus is a technical spec, and the
 * subject matter of one is full of credentials, deletions and organization settings it only
 * describes. What Q7 names is an act somebody is asked to make, so the signals of those kinds
 * are a request shape - somebody, a verb, the thing - and not the lone noun: measured over the
 * 985 post-cut cases, the lone nouns hold 342 of them and the request shapes hold 94.
 */
const DIRECTED_AT = "(you|he|she|they|a person|the person|a human|an owner|an admin)";

/** A request of one of `verbs` upon one of `objects`, directed at a person, in one sentence. */
function askedOf(verbs: string, objects: string): RegExp {
  return new RegExp(
    `\\b${DIRECTED_AT}\\b[^.\\n]{0,60}\\b(${verbs})\\b[^.\\n]{0,40}\\b(${objects})\\b`,
    "i"
  );
}

/** What a credential is called, wherever one of these kinds names one. */
const CREDENTIAL =
  "credential|credentials|api[- ]key|access key|private key|ssh key|password|token|secret";

/**
 * The kinds and their signals, in the order they are tried. A signal is matched over the whole
 * rendered evidence, case-insensitively, and names itself in the reason.
 */
export const NEVER_DECIDE_RULES: readonly NeverDecideRule[] = [
  {
    kind: "destructive-act",
    what: "a document that asks for a destructive act",
    signals: [
      { name: "rm -rf", pattern: /\brm\s+-[a-z]*r[a-z]*f?\b/i },
      { name: "a force push", pattern: /\bforce[- ]push|push\s+--force\b/i },
      { name: "a hard reset of a branch", pattern: /\breset\s+--hard\b/i },
      { name: "dropping a table or a database", pattern: /\bdrop\s+(table|database|schema)\b/i },
      { name: "truncating a table", pattern: /\btruncate\s+(table|the)\b/i },
      {
        name: "deleting or destroying a store of data",
        pattern:
          /\b(delete|deleting|destroy|destroying|wipe|wiping|purge|purging|erase|erasing)\s+(the\s+|every\s+|all\s+|its\s+)?(database|databases|volume|volumes|bucket|buckets|namespace|cluster|snapshot|snapshots|backup|backups|repository|repositories)\b/i,
      },
      { name: "tearing down an environment", pattern: /\btear(ing)?\s+down\s+the\b/i },
      {
        name: "revoking an access somebody holds",
        pattern: /\brevok(e|es|ing)\s+(the\s+|their\s+|a\s+)?(access|grant|membership|permission)/i,
      },
      { name: "an act named as irreversible", pattern: /\b(irreversible|cannot be undone)\b/i },
    ],
  },
  {
    kind: "credential",
    what: "a document that asks for a credential",
    signals: [
      {
        name: "a person asked to make or hand over a credential",
        pattern: askedOf(
          "create|creates|make|makes|mint|mints|issue|issues|generate|generates|rotate|rotates|paste|pastes|hand|hands|give|gives|share|shares|provide|provides|upload|uploads|install|installs",
          CREDENTIAL
        ),
      },
      {
        name: "a request put to a person for a credential",
        pattern: new RegExp(
          `\\b(ask|asks|asking|asked)\\s+(you|him|her|them|a person|the person|an owner)\\b[^.\\n]{0,60}\\b(${CREDENTIAL})\\b`,
          "i"
        ),
      },
    ],
  },
  {
    kind: "owner-only-setting",
    what: "a document that asks for a setting only an organization owner can make",
    signals: [
      {
        name: "an act only an owner can make",
        pattern:
          /\bonly (an?|the) (org|organization|organisation|github|repository|repo)? ?(owner|admin|administrator)s? (can|could|may|must|has to)\b/i,
      },
      {
        name: "an organization owner who must act",
        pattern:
          /\b(org|organization|organisation) (owner|admin)s?\b[^.\n]{0,40}\b(must|has to|have to|needs to|need to|can|only)\b/i,
      },
      {
        name: "a person asked to install an app or change an organization setting",
        pattern: askedOf(
          "install|installs|enable|enables|turn on|turns on|change|changes|set|sets",
          "github app|app|apps|organization setting|org setting|sso|saml|billing|seat|seats|policy"
        ),
      },
    ],
  },
  {
    kind: "outside-message",
    what: "a document that asks for a message to somebody outside the company",
    signals: [
      {
        name: "somebody outside the company",
        pattern:
          /\b(outside the (company|organisation|organization)|(somebody|someone|a person) outside)\b/i,
      },
      {
        name: "a person asked to write to a customer, a vendor or an upstream project",
        pattern: askedOf(
          "email|emails|message|messages|write|writes|reply|replies|tell|tells|contact|contacts|ask|asks",
          "customer|customers|client|clients|vendor|vendors|supplier|upstream|outside the company"
        ),
      },
      {
        name: "a public announcement",
        pattern: /\b(press release|announce publicly|post publicly|publish publicly)\b/i,
      },
    ],
  },
  {
    kind: "outside-account",
    what: "a document that asks for an account on an outside platform",
    signals: [
      {
        name: "a person asked to open an account",
        pattern: askedOf(
          "sign up|signs up|create|creates|open|opens|register|registers",
          "account|tenant|workspace|subscription"
        ),
      },
      {
        name: "an account on an outside platform",
        pattern: /\baccount on (an|the|a) (outside|third[- ]party|external)\b/i,
      },
    ],
  },
  {
    kind: "changes-these-skills",
    what: "a document that changes these skills or their numbers",
    signals: [
      {
        name: "the approval skill's own files",
        pattern: /\bskills?\/approval|skill:\/\/approval/i,
      },
      { name: "the never-decide rule", pattern: /\bnever[- ]decide\b/i },
      { name: "the approval eval's own scripts", pattern: /\bapproval-eval\b/i },
    ],
  },
];

/**
 * Every kind the rule can hold, in the order it tries them: the parked root first, whose test
 * is the issue's own facts rather than a signal of the text.
 */
export const NEVER_DECIDE_KINDS: readonly NeverDecideKind[] = [
  "parked-legion-root",
  ...NEVER_DECIDE_RULES.map((rule) => rule.kind),
];

/** The artifact ids this case asks to approve. */
function artifactsOf(row: CaseRow | null | undefined): string[] {
  const ids: string[] = [];
  for (const document of row?.approval?.documents ?? []) {
    if (document.artifactId !== null) ids.push(document.artifactId);
  }
  return ids;
}

/**
 * Whether this case is an approval of the primary document of a parked root carrying the
 * `legion` label, which the daemon reads as the person's own instruction to run that tree. The
 * issue's facts come from the caller; a case with none is not held under this kind.
 */
function parkedLegionRoot(input: NeverDecideInput): string | null {
  const issue = input.issue;
  if (!issue) return null;
  if (issue.parent !== null) return null;
  if (!issue.labels.includes("legion")) return null;
  if (!PARKED.includes(issue.status)) return null;
  const primary = issue.primaryArtifactId;
  if (primary === null) return null;
  const row = input.row;
  // With a case in hand, the hold stands only on an approval of that primary document. With a
  // bare document, the issue's facts alone hold it, which is the conservative reading.
  if (row !== null && row !== undefined) {
    if (row.kind !== "approval") return null;
    if (!artifactsOf(row).includes(primary)) return null;
  }
  return (
    `the issue is a root carrying the legion label, parked at ${issue.status}, ` +
    "and this is its primary document"
  );
}

/**
 * What the rule says about one case or one document. The first kind whose signal fires holds it,
 * and the reason names that signal.
 */
export function neverDecide(input: NeverDecideInput): NeverDecideAnswer {
  const parked = parkedLegionRoot(input);
  if (parked !== null) {
    return { held: true, kind: "parked-legion-root", reason: parked };
  }
  for (const rule of NEVER_DECIDE_RULES) {
    for (const signal of rule.signals) {
      if (signal.pattern.test(input.text)) {
        return { held: true, kind: rule.kind, reason: `${rule.what}: it names ${signal.name}` };
      }
    }
  }
  return { held: false, kind: null, reason: "no kind the agent never decides was named" };
}
