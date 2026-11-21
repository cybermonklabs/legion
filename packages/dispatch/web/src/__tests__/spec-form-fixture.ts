import { ApiError } from "../api/client";

const findingLines = [
  "- statement ship-at-head in spec.md version 12: it has 3 sentences, and a statement has one or two.",
  '- claim v-ship in spec.md version 12: ASD-STE100 does not permit a semicolon: ";".',
].join("\n");

/**
 * What Dispatch answers a person's approval of a document that breaks the form of The Spec, as
 * its 409 `SPEC_FORM` body carries it: `error` names each text and the person's next step, which
 * `refusal` and `next_step` hold apart, and `changes_reason` is the reason a request for changes
 * then sends to the author.
 */
const refusal = `Dispatch refused this approval, because 2 texts break the form of The Spec.\n${findingLines}`;
const nextStep = "To send these findings to the author, choose Request changes.";
export const specFormRefusalBody = {
  changes_reason: `Dispatch refused my approval, because 2 texts break the form of The Spec.\n${findingLines}\nFix each text, then request approval again.`,
  code: "SPEC_FORM",
  error: `${refusal}\n${nextStep}`,
  findings: [
    {
      artifact_id: "artifact-1",
      detail: "it has 3 sentences, and a statement has one or two",
      document: "spec.md",
      excerpt: "Dispatch ships at the head. It ships once. It ships again.",
      kind: "statement",
      node: "ship-at-head",
      rule: "sentences",
      version: 12,
      where: "spec.md version 12",
      ways: "",
    },
    {
      artifact_id: "artifact-1",
      detail: 'ASD-STE100 does not permit a semicolon: ";"',
      document: "spec.md",
      excerpt: "The job ships; it stops.",
      kind: "claim",
      node: "v-ship",
      rule: "ste100-semicolon",
      version: 12,
      where: "spec.md version 12",
      ways: "",
    },
  ],
  findings_total: 2,
  next_step: nextStep,
  refusal,
};

/** That refusal, as the client raises it. */
export function specFormRefusal(): ApiError {
  return new ApiError(409, specFormRefusalBody);
}
