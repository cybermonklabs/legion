import type { ChangeRequestNode, ChangeRequestRead, SpecNodeRead } from "../../api/types";

/** The issue these fixtures read, built at run time from a project and a number, as Dispatch
 *  names one. */
export const ISSUE = `${"CORE"}-${7}`;

/** The repository the issue's project names as its spec source. */
export const REPO = "acme/app";

/** A change request node as `GET /api/v1/issues/{key}/change-request` answers it. */
export function requestNode(
  overrides: Partial<ChangeRequestNode> & Pick<ChangeRequestNode, "id">
): ChangeRequestNode {
  return {
    approved_text: "",
    base: 0,
    block: `${ISSUE}/spec#${overrides.id}`,
    current: 0,
    kind: "statement",
    text: "",
    what: "added",
    ...overrides,
  };
}

/** The change request of version 3 of the issue's spec: one statement changed, one added and one
 *  removed, and one check changed, with the digests an approval of version 3 and of version 2 of
 *  the Verification document pins. */
export function changeRequest(overrides: Partial<ChangeRequestRead> = {}): ChangeRequestRead {
  return {
    changed_checks: [
      requestNode({
        approved_text:
          ':::check{#a-second-payment-is-refused serves="an-order-is-paid-once" subject="code" procedure="program"}\nClaim: A second payment is refused.\n:::\n',
        base: 1,
        block: `${ISSUE}/verification#a-second-payment-is-refused`,
        current: 1,
        id: "a-second-payment-is-refused",
        kind: "check",
        text: ':::check{#a-second-payment-is-refused statement="an-order-is-paid-once" subject="code" procedure="program" base="1"}\nClaim: A second payment is refused at once.\n\nProcedure: The test pays one order two times.\n:::',
        what: "changed",
      }),
    ],
    changes_checks: true,
    conclusion: null,
    digest: "a".repeat(64),
    final_list: null,
    issue: ISSUE,
    judgment: "none",
    judgment_reasons: [],
    problems: [],
    proposals: [
      requestNode({
        approved_text:
          ':::requirement{#an-order-is-paid-once kind="behavior" parent="billing-works" about="billing"}\nAn order is paid once.\n:::\n',
        base: 1,
        current: 1,
        id: "an-order-is-paid-once",
        text: ':::requirement{#an-order-is-paid-once kind="behavior" under="billing-works" about="billing" serves="b-k1" base="1"}\nAn order is paid once, and a second payment is refused.\n:::',
        what: "changed",
      }),
      requestNode({
        id: "a-refund-returns-the-payment",
        text: ':::requirement{#a-refund-returns-the-payment kind="behavior" about="billing"}\nA refund returns the payment of an order.\n:::',
        what: "added",
      }),
      requestNode({
        approved_text:
          ':::requirement{#a-bill-names-its-order kind="behavior" about="billing"}\nA bill names its order.\n:::\n',
        base: 1,
        current: 1,
        id: "a-bill-names-its-order",
        text: ':::requirement{#a-bill-names-its-order kind="behavior" about="billing" base="1" removes="true"}\nThis statement goes.\n:::',
        what: "removed",
      }),
    ],
    proposes: true,
    repo: REPO,
    spec_source: true,
    verification_digest: "b".repeat(64),
    verification_version: 2,
    version: 3,
    ...overrides,
  };
}

/** The change request of an issue whose repository is no spec source. */
export function noSourceRequest(): ChangeRequestRead {
  return changeRequest({
    changed_checks: [],
    changes_checks: false,
    proposals: [],
    proposes: false,
    repo: "acme/elsewhere",
    spec_source: false,
    verification_digest: "",
    verification_version: 0,
  });
}

/** One approved node as `GET /api/v1/projects/{key}/spec/{owner}/{repo}/nodes/{id}` answers it:
 *  `blocks` its versions oldest first, the last written by `approval` (the issue, its spec's
 *  version and digest, the request and the person), each earlier one by another issue. */
export function approvedNode(
  id: string,
  kind: string,
  blocks: readonly string[],
  approval: { issue: string; version: number; digest: string; ask: string; at: string },
  removal?: { version: number; at: string }
): SpecNodeRead {
  const provenance = (issue: string, version: number, digest: string, ask: string) => ({
    act: "approval",
    ask,
    issue,
    person: { id: "alice", kind: "user" as const },
    spec: { artifact_id: `${issue}-spec`, digest, version },
  });
  return {
    id,
    kind,
    project: "CORE",
    removals:
      removal === undefined
        ? []
        : [
            {
              at: removal.at,
              provenance: provenance(
                approval.issue,
                approval.version,
                approval.digest,
                approval.ask
              ),
              version: removal.version,
            },
          ],
    removed: removal !== undefined,
    repo: REPO,
    versions: blocks.map((block, index) => {
      const last = index === blocks.length - 1 && removal === undefined;
      return {
        at: last ? approval.at : "2026-11-01T10:00:00Z",
        block,
        provenance: last
          ? provenance(approval.issue, approval.version, approval.digest, approval.ask)
          : provenance(`${"CORE"}-${1}`, 1, "c".repeat(64), "ask-seed"),
        version: index + 1,
      };
    }),
  };
}
