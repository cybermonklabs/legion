import { expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import {
  canonicalRole,
  canonicalTopic,
  claimProject,
  issueEventKey,
  issueProject,
  legacyTwin,
  parseRoleClaimKey,
  roleClaimKey,
  roleProject,
  subjectProject,
  subjectProjectFamilies,
  topicForms,
} from "./subject-project";

// One table, fixtures/subject-project.json, which the Go readers run too
// (packages/envoy/internal/contracts/subject_project_test.go): a case added for either language
// is run by both, so the two readers of a message name cannot drift apart.
const fixture = JSON.parse(
  readFileSync(path.join(import.meta.dir, "../fixtures/subject-project.json"), "utf8")
) as {
  families: string[];
  issueProject: { key: string; project: string; ok: boolean }[];
  roleProject: { role: string; project: string }[];
  subjectProject: { subject: string; project: string; repo: string; ok: boolean }[];
  canonicalTopic: { topic: string; canonical: string; legacy: boolean }[];
  legacyTwin: { canonical: string; twin: string | null }[];
  canonicalRole: { role: string; project: string; name: string; legacy: boolean }[];
  topicForms: { topic: string; forms: string[] }[];
  roleClaimKey: { project: string; role: string; key: string }[];
  issueEventKey: { subject: string; key: string; legacy: boolean; ok: boolean }[];
  claimProject: { role: string; requested: string; project?: string; error?: boolean }[];
};

// Each section held at least this many rows when this was written: a read that finds a handful,
// or none, passes while covering nothing.
const least = {
  families: 15,
  issueProject: 8,
  roleProject: 10,
  subjectProject: 38,
  canonicalTopic: 23,
  legacyTwin: 20,
  canonicalRole: 8,
  topicForms: 7,
  roleClaimKey: 9,
  issueEventKey: 13,
  claimProject: 12,
} as const;

for (const [section, count] of Object.entries(least)) {
  test(`the shared fixture's ${section} section holds its rows`, () => {
    expect(fixture[section as keyof typeof least].length).toBeGreaterThanOrEqual(count);
  });
}

test("an issue key names its project, and nothing else does", () => {
  for (const row of fixture.issueProject) {
    expect(issueProject(row.key), row.key).toBe(row.ok ? row.project : undefined);
  }
});

test("a Legion role names the project of its token, every other role none", () => {
  for (const row of fixture.roleProject) {
    expect(roleProject(row.role), row.role).toBe(row.project);
  }
});

test("each family's message name is read for its project", () => {
  for (const row of fixture.subjectProject) {
    expect(subjectProject(row.subject), row.subject).toEqual({
      project: row.project,
      repo: row.repo,
      ok: row.ok,
    });
  }
});

test("an old-form topic reads as its new form, and every other topic as itself", () => {
  for (const row of fixture.canonicalTopic) {
    expect(canonicalTopic(row.topic), row.topic).toEqual({
      canonical: row.canonical,
      legacy: row.legacy,
    });
  }
});

test("a new-form topic names its old form while the old form is read", () => {
  for (const row of fixture.legacyTwin) {
    expect(legacyTwin(row.canonical), row.canonical).toBe(row.twin ?? undefined);
  }
});

test("each old-form topic and its new form are each other's twins", () => {
  for (const row of fixture.canonicalTopic.filter((candidate) => candidate.legacy)) {
    expect(legacyTwin(row.canonical), row.topic).toBe(row.topic);
  }
});

test("a claimed role reads in either form", () => {
  for (const row of fixture.canonicalRole) {
    expect(canonicalRole(row.role), row.role).toEqual({
      project: row.project,
      name: row.name,
      legacy: row.legacy,
    });
  }
});

test("a topic's forms are itself and the other form of its name", () => {
  for (const row of fixture.topicForms) {
    expect(topicForms(row.topic), row.topic).toEqual(row.forms);
  }
});

test("a role claim is keyed by its bare name unless its project differs, and reads back", () => {
  for (const row of fixture.roleClaimKey) {
    expect(roleClaimKey(row.project, row.role), `${row.project} ${row.role}`).toBe(row.key);
    expect(parseRoleClaimKey(row.key), row.key).toEqual({ project: row.project, role: row.role });
  }
});

test("an issue event's name carries its key in either form, and any other name none", () => {
  for (const row of fixture.issueEventKey) {
    expect(issueEventKey(row.subject), row.subject).toEqual(
      row.ok ? { key: row.key, legacy: row.legacy } : undefined
    );
  }
});

test("a claim's project is its role's, or the one it asks for when the name has none", () => {
  for (const row of fixture.claimProject) {
    const answer = claimProject(row.role, row.requested);
    if (row.error === true) {
      expect(answer, `${row.role} asking for ${row.requested}`).toBeInstanceOf(Error);
      continue;
    }
    expect(answer, `${row.role} asking for ${row.requested}`).toBe(row.project as string);
  }
});

test("the families the reader has a row for are the shared fixture's, in its order", () => {
  expect(subjectProjectFamilies()).toEqual(fixture.families);
});
