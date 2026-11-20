import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { dispatchCliLive, useDispatchCliDirectories } from "../../test/dispatch-cli-live";
import {
  type DispatchStack,
  dispatchStackEnabled,
  startDispatchStack,
} from "../../test/dispatch-stack";

// A person's approval pins the bytes of the version they approved, and the real `dispatch` CLI of
// this checkout, against a real Dispatch server of this checkout, names those bytes. The server's
// Go rows (TestEveryApprovalReadNamesTheApprovedBytes, reviews_content_test.go) prove the hash
// each approval read carries; none proves that it is the hash of the version's UTF-8 bytes as a
// reader fetches them over HTTP, that it stays on the version the person approved once a later
// version is written, and that the CLI prints the same hash a reader computes from the text it
// fetched, which is what a reviewer compares before trusting an approval. The spec holds text
// outside ASCII, so a hash of any other encoding of the same text reads differently.

/** The person who owns the project and approves the spec, as the server's header identity names
 *  them. */
const PERSON = "alice";
/** The session the CLI acts as. */
const SESSION = "session-approved-version-bytes-live";
/** The project the row makes its issue in. */
const PROJECT = "PINNED";
/** The spec the person approves: accents, a dash, CJK and an emoji, each more than one UTF-8 byte. */
const SPEC =
  "# Café design — naïve 東京 plan\n\nThe façade ships first 🚀, then the résumé page.\n";

/** One version as `GET /api/v1/artifacts/{id}/versions/{n}` answers it. */
interface FetchedVersion {
  readonly markdown: string;
  readonly sha256: string;
}

/** One review as `GET /api/v1/artifacts/{id}/reviews` lists it. */
interface RecordedReview {
  readonly state: string;
  readonly kind: string;
  readonly version: number;
  readonly content_sha256: string;
}

/** A document as `GET /api/v1/artifacts/{id}` answers it: its approval alone. */
interface ApprovedDocument {
  readonly approval: { readonly state: string; readonly content_sha256?: string };
}

/** The SHA-256 of text's UTF-8 bytes, which is what a reader that fetched the text computes. */
function utf8Sha256(text: string): string {
  return createHash("sha256").update(Buffer.from(text, "utf8")).digest("hex");
}

// Reserved here, at the file's module scope: the file's own `afterAll` removes each once the
// `describe` below has stopped the server writing into it.
const directories = useDispatchCliDirectories();

describe.skipIf(!dispatchStackEnabled)(
  "the bytes a person's approval pins, as the real CLI names them against a real Dispatch",
  () => {
    let stack: DispatchStack | undefined;
    const { asPerson, createIssue, docEdit, docRead } = dispatchCliLive({
      stack: () => stack,
      session: SESSION,
      person: PERSON,
      spec: SPEC,
      directories,
    });

    beforeAll(async () => {
      stack = await startDispatchStack({ work: directories.stack });
      await asPerson("POST", "/api/v1/projects", { key: PROJECT, name: `${PROJECT} project` });
    }, 600_000);
    afterAll(async () => {
      await stack?.stop();
    }, 60_000);

    test("the approval names the approved version's UTF-8 bytes, and keeps them after a new version", async () => {
      const issue = await createIssue(PROJECT, "Approve the design whose text is not ASCII");
      const spec = issue.primary_artifact_id;
      const versionRoute = (version: number) => `/api/v1/artifacts/${spec}/versions/${version}`;

      const first = await asPerson<FetchedVersion>("GET", versionRoute(1));
      const approvedBytes = utf8Sha256(first.markdown);
      expect(first.markdown).toContain("東京");
      // The control: the same text hashed as UTF-16 code units reads otherwise, so a hash of
      // anything but the UTF-8 bytes would fail the comparisons below.
      expect(
        createHash("sha256").update(Buffer.from(first.markdown, "utf16le")).digest("hex")
      ).not.toBe(approvedBytes);
      expect(first.sha256).toBe(approvedBytes);

      await asPerson("POST", `/api/v1/artifacts/${spec}/reviews`, { state: "approved" });
      const approved = await asPerson<RecordedReview[]>("GET", `/api/v1/artifacts/${spec}/reviews`);
      expect(approved.map((review) => [review.state, review.kind, review.version])).toEqual([
        ["approved", "person", 1],
      ]);
      expect(approved[0]?.content_sha256).toBe(approvedBytes);

      // A later version the person has not approved: plain prose, so no settlement stores a
      // version of its own beside it.
      await docEdit(issue.key, spec, "A new line the person has not approved.");
      const second = await asPerson<FetchedVersion>("GET", versionRoute(2));
      const latestBytes = utf8Sha256(second.markdown);
      expect(latestBytes).not.toBe(approvedBytes);
      expect(second.sha256).toBe(latestBytes);

      const reviews = await asPerson<RecordedReview[]>("GET", `/api/v1/artifacts/${spec}/reviews`);
      expect(reviews[0]?.content_sha256).toBe(approvedBytes);
      const document = await asPerson<ApprovedDocument>("GET", `/api/v1/artifacts/${spec}`);
      expect(document.approval).toMatchObject({ state: "stale", content_sha256: approvedBytes });

      const approvedRead = await docRead(issue.key, spec, 1);
      expect(approvedRead).toContain(`Content: sha256:${approvedBytes}`);
      expect(approvedRead).toContain(`content sha256:${approvedBytes}`);
      const latestRead = await docRead(issue.key, spec, 2);
      expect(latestRead).toContain(`Content: sha256:${latestBytes}`);
    }, 120_000);
  }
);
