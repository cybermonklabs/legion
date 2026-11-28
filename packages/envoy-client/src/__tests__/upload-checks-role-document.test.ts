import { describe, expect, test } from "bun:test";
import type { Artifact } from "@legion/contracts";
import { artifactKindConflictRule } from "@legion/contracts";
import { artifactNameExistsProblem } from "../dispatch-commands/upload-checks";

// Dispatch takes a Markdown upload under the name of an issue's plan, Verification document or
// decisions document that an earlier upload stored as a file or a picture as that artifact's next
// version, and makes it the document (R15, storeArtifact in
// packages/envoy/internal/dispatch/api/artifacts.go). So `dispatch artifact`'s refusal of such an
// upload advises the replacement, never another name. The upload that makes the document names no
// token: Dispatch refuses one 409 PRECONDITION_FAILED, since no document stands there yet, so the
// refusal advises --force alone, whatever the file's version states. Every other change of kind
// Dispatch refuses under the name, and so is still told to take another name.

const OWNER = "issue DSP-1";

/** An artifact `slug` (named `name`) of kind `kind`, owned by issue DSP-1 or, for `issueKey` null,
 *  by project DSP, at version 1, whose version states `token` where one is given. */
function heldArtifact(
  slug: string,
  name: string,
  kind: Artifact["kind"],
  issueKey: string | null,
  token?: string
): Artifact {
  return {
    id: `artifact-${slug}`,
    issue_key: issueKey,
    project: "DSP",
    session_id: null,
    ref_key: `${issueKey ?? "DSP"}/${slug}`,
    slug,
    name,
    kind,
    primary: false,
    created_by: { kind: "session", id: "ses-0" },
    created_at: "2026-11-24T00:00:00Z",
    versions: [
      {
        number: 1,
        named: false,
        summary: null,
        authors: [{ kind: "session", id: "ses-0" }],
        created_at: "2026-11-24T00:00:00Z",
        ...(token === undefined ? {} : { token }),
      },
    ],
  };
}

const TOKEN = `sha256:${"a".repeat(64)}`;

const ROLE_DOCUMENTS = [
  ["plan-md", "plan.md"],
  ["verification-md", "verification.md"],
  ["decisions-md", "decisions.md"],
] as const;

const HELD_AS = [
  ["a file whose version states a token", "file", TOKEN],
  ["a file whose version states no token", "file", undefined],
  ["a picture whose version states a token", "image", TOKEN],
] as const;

describe("artifactNameExistsProblem for a Markdown upload over an issue's role document held as a file", () => {
  test.each(
    ROLE_DOCUMENTS.flatMap(([slug, name]) =>
      HELD_AS.map(([label, kind, token]) => [slug, label, name, kind, token] as const)
    )
  )("%s held as %s advises --force alone and no other name", (slug, _label, name, kind, token) => {
    const text = artifactNameExistsProblem(
      heldArtifact(slug, name, kind, "DSP-1", token),
      OWNER,
      "doc"
    );
    expect(text).toStartWith(
      `${OWNER} already holds ${name} (slug ${slug}, kind ${kind}) at version 1, which session ses-0 wrote.`
    );
    expect(text).toContain("(--force) alone");
    expect(text).toContain(`next version and makes the ${kind === "image" ? "picture" : "file"}`);
    expect(text).not.toContain("--precondition-json");
    expect(text).not.toContain("sha256:");
    expect(text).not.toContain("another name");
    expect(text).not.toContain("with force or without");
    expect(text).not.toContain("doc-edit");
  });
});

describe("artifactNameExistsProblem for every other upload of another kind than the name holds", () => {
  test.each([
    [
      "a Markdown upload over a project's plan-md held as a file",
      heldArtifact("plan-md", "plan.md", "file", null, TOKEN),
      "doc",
    ],
    [
      "a Markdown upload over an issue's notes-md held as a file",
      heldArtifact("notes-md", "notes.md", "file", "DSP-1", TOKEN),
      "doc",
    ],
    [
      "a file over an issue's plan-md held as a document",
      heldArtifact("plan-md", "plan.md", "doc", "DSP-1"),
      "file",
    ],
    [
      "a picture over an issue's plan-md held as a document",
      heldArtifact("plan-md", "plan.md", "doc", "DSP-1"),
      "image",
    ],
  ] as const)("%s is told to take another name", (_label, held, upload) => {
    const text = artifactNameExistsProblem(held, OWNER, upload);
    expect(text).toEndWith(` ${artifactKindConflictRule(held.kind, upload)}`);
    expect(text).toContain("Give the upload another name.");
    expect(text).not.toContain("--force");
  });
});
