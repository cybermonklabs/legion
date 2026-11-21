import { expect, test } from "bun:test";

import { createApiClient, type DispatchApiClient, type FetchImplementation } from "./client";

// An upload over a document the owner already holds names the token of the version it replaces
// (INFRA-1503). The client builds that upload as JSON for text and as a multipart form for a file,
// and each shape must carry the token as the server reads it: the JSON body's `precondition`
// object, and the form's `precondition` field holding that object's JSON. A token dropped from
// either shape is refused by this server, and a server before it would replace the document
// unchecked. These rows read the body the client handed to fetch, below every spy a page test
// puts on `api.uploadArtifact`.

const TOKEN = "sha256:the-version-this-upload-replaces";

/** A client whose every request is answered as a stored upload, and the request it was sent. */
function recordingClient(): {
  api: DispatchApiClient;
  sent: () => { path: string; body: unknown };
} {
  let last: { path: string; body: unknown } | undefined;
  const fetchImpl: FetchImplementation = async (input, init) => {
    last = { path: input.toString(), body: init?.body };
    return Response.json(
      {
        artifact: { id: "artifact-1", issue_key: "CORE-1", name: "notes.md", slug: "notes-md" },
        version: { number: 2 },
      },
      { status: 201 }
    );
  };
  return {
    api: createApiClient(fetchImpl),
    sent: () => {
      if (last === undefined) throw new Error("the client sent no request");
      return last;
    },
  };
}

const bounds = { key: "upload-body-test" };
const markdown = () =>
  new File(["# Notes\n\nSecond version.\n"], "notes.md", { type: "text/markdown" });

test("a text upload carries the token of the version it replaces in its JSON body", async () => {
  const { api, sent } = recordingClient();
  await api.uploadArtifact(
    { issue: "CORE-1" },
    {
      content: "# Notes\n\nSecond version.\n",
      name: "notes.md",
      precondition: { document: TOKEN },
    },
    bounds
  );
  const { body, path } = sent();
  expect(path).toEndWith("/api/v1/issues/CORE-1/artifacts");
  expect(typeof body).toBe("string");
  expect(JSON.parse(body as string).precondition).toEqual({ document: TOKEN });
});

test("a file upload carries the token as the form's precondition field", async () => {
  const { api, sent } = recordingClient();
  await api.uploadArtifact(
    { project: "CORE" },
    { file: markdown(), name: "notes.md", precondition: { document: TOKEN } },
    bounds
  );
  const { body, path } = sent();
  expect(path).toEndWith("/api/v1/projects/CORE/artifacts");
  expect(body).toBeInstanceOf(FormData);
  expect((body as FormData).get("precondition")).toBe(JSON.stringify({ document: TOKEN }));
});

// An upload naming no token sends no precondition field: the server answers an empty field as none,
// and reads any other value as a token, refusing one that is not (multipartJSONField,
// artifacts.go:353-357).
test("an upload that names no token sends no precondition, in either shape", async () => {
  const text = recordingClient();
  await text.api.uploadArtifact(
    { issue: "CORE-1" },
    { content: "# New\n", name: "new.md" },
    bounds
  );
  expect(Object.keys(JSON.parse(text.sent().body as string))).not.toContain("precondition");

  const file = recordingClient();
  await file.api.uploadArtifact({ issue: "CORE-1" }, { file: markdown(), name: "new.md" }, bounds);
  expect((file.sent().body as FormData).has("precondition")).toBe(false);
});
