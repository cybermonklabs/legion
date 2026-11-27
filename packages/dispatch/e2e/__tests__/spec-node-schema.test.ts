// The schema of a node of The Spec, and the YAML of the spec files, read the same in TypeScript as
// in Go (R5 of the specification). ajv reads the one schema of the repository and the one table
// of its cases that the Go test TestSchemaCases reads; the yaml library reads each YAML file of
// specgraph's testdata/nodes, which the printer writes (TestPrintWritesTheFileForm), each as the
// data of the .decoded.json file beside it, which a person wrote and which the Go test
// TestNodesDecodeToTheirData holds yaml.v3 to: hard-texts.yaml holds each line that the printer
// quotes or escapes, such as `=`, `<<`, `yes`, `0x_` and a date with a time. It also reads each YAML
// fixture of specgraph's testdata, each file that the printer wrote for the tests, as the data of
// testdata/fixtures.decoded.json, which the Go test TestEachYAMLFixtureDecodesAsItsData writes and
// holds yaml.v3 and the Go schema validator to.

import { describe, expect, test } from "bun:test";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020";
import { parseAllDocuments } from "yaml";

/** The path of path under packages/envoy/internal/dispatch/specgraph. */
function specgraphPath(path: string): string {
  return fileURLToPath(
    new URL(`../../../envoy/internal/dispatch/specgraph/${path}`, import.meta.url)
  );
}

/** The text of path under packages/envoy/internal/dispatch/specgraph. */
function specgraph(path: string): string {
  return readFileSync(specgraphPath(path), "utf8");
}

interface SchemaCase {
  name: string;
  node: unknown;
  valid: boolean;
  at: string[];
}

const validate = new Ajv2020({ allErrors: true, strict: true }).compile(
  JSON.parse(specgraph("schema/spec-node.schema.json"))
);
const cases = JSON.parse(specgraph("testdata/schema-cases.json")) as SchemaCase[];

describe("the schema of a node", () => {
  test("the table of cases holds rows", () => {
    expect(cases.length).toBeGreaterThan(0);
  });
  for (const row of cases) {
    test(row.name, () => {
      expect(validate(row.node), row.name).toBe(row.valid);
      if (row.valid) return;
      // ajv names each `if` whose `then` or `else` failed, beside the failure itself; the Go
      // validator names the failure alone.
      const at = new Set(
        (validate.errors ?? []).filter((e) => e.keyword !== "if").map((e) => e.instancePath)
      );
      expect([...at].sort(), row.name).toEqual([...new Set(row.at)].sort());
    });
  }
});

describe("the YAML of the spec files", () => {
  const nodeFiles = readdirSync(specgraphPath("testdata/nodes"))
    .filter((name) => name.endsWith(".yaml"))
    .map((name) => name.slice(0, -".yaml".length))
    .sort();
  test("testdata/nodes holds YAML files", () => {
    expect(nodeFiles.length).toBeGreaterThan(0);
  });
  for (const name of nodeFiles) {
    test(`${name}.yaml reads as ${name}.decoded.json, and the schema takes each node`, () => {
      const documents = parseAllDocuments(specgraph(`testdata/nodes/${name}.yaml`));
      if ("empty" in documents) throw new Error(`${name}.yaml holds no document`);
      const want = JSON.parse(specgraph(`testdata/nodes/${name}.decoded.json`)) as unknown[];
      expect(documents.length).toBe(want.length);
      documents.forEach((document, i) => {
        expect(document.errors, `${name}.yaml, document ${i + 1}`).toEqual([]);
        expect(document.warnings, `${name}.yaml, document ${i + 1}`).toEqual([]);
        const data: unknown = document.toJS();
        expect(data, `${name}.yaml, document ${i + 1}`).toEqual(want[i]);
        expect(validate(data), `${name}.yaml, document ${i + 1}`).toBe(true);
      });
    });
  }
});

interface DecodedDocument {
  data: unknown;
  schema: boolean;
}

describe("each YAML fixture of specgraph", () => {
  const decoded = JSON.parse(specgraph("testdata/fixtures.decoded.json")) as Record<
    string,
    DecodedDocument[]
  >;
  const fixtures = readdirSync(specgraphPath("testdata"), { recursive: true, encoding: "utf8" })
    .filter((name) => name.endsWith(".yaml"))
    .sort();
  test("fixtures.decoded.json names each YAML fixture", () => {
    expect(fixtures.length).toBeGreaterThan(0);
    expect(fixtures).toEqual(Object.keys(decoded).sort());
  });
  for (const name of fixtures) {
    test(`${name} reads as yaml.v3 reads it, and the schema gives the verdict of Go`, () => {
      const documents = parseAllDocuments(specgraph(`testdata/${name}`));
      if ("empty" in documents) throw new Error(`${name} holds no document`);
      const want = decoded[name] ?? [];
      expect(documents.length, name).toBe(want.length);
      documents.forEach((document, i) => {
        expect(document.errors, `${name}, document ${i + 1}`).toEqual([]);
        expect(document.warnings, `${name}, document ${i + 1}`).toEqual([]);
        const data: unknown = document.toJS();
        expect(data, `${name}, document ${i + 1}`).toEqual(want[i]?.data);
        expect(validate(data), `${name}, document ${i + 1}`).toBe(want[i]?.schema);
      });
    });
  }
});
