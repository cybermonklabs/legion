import { expect, test } from "bun:test";

import type { BlockElementVersions, BlockSchema } from "../../api/types";
import { compiledBlockSchema } from "./schema";
import { schemaCreatable } from "./schema-creatable";

/** The compiled manifest as the build that introduces one more typed block, "future", declares
 *  it: the version raised by one, and the block read from that version and created from it
 *  (`creatable`) or from the version after it. */
function withFutureBlock(creatable: boolean): BlockSchema {
  const version = compiledBlockSchema.version + 1;
  return {
    ...compiledBlockSchema,
    types: [
      ...compiledBlockSchema.types,
      {
        attributes: {},
        content: "block+",
        creatable_since: creatable ? version : version + 1,
        name: "future",
        render: "host",
        since: version,
      },
    ],
    version,
  };
}

/** The manifest with its version raised by one and nothing else changed: the next release's
 *  build, which creates each element this one reads and creates only from that version. */
function raisedByOne(schema: BlockSchema): BlockSchema {
  return { ...schema, version: schema.version + 1 };
}

/** The manifest with the named attributes of the typed block `type` taken out. */
function withoutAttributes(schema: BlockSchema, type: string, names: string[]): BlockSchema {
  return {
    ...schema,
    types: schema.types.map((declared) =>
      declared.name === type
        ? {
            ...declared,
            attributes: Object.fromEntries(
              Object.entries(declared.attributes).filter(([name]) => !names.includes(name))
            ),
          }
        : declared
    ),
  };
}

// The page presents the manifest it was built with: its version V, and R, the highest `since`
// among the elements it may create (`creatable_since` V or less), as Go's `pmdoc.SchemaCreatable`
// computes it. Today's manifest creates every element it reads but the check block's `subject`
// and `program`, which it holds uncreatable until the release that records real deploys, and its
// R is the highest `since` of the rest, whatever that is: the numbers come from the manifest,
// never from a literal here, so raising the manifest does not break this row.
test("the compiled manifest is the page's own, and it creates everything it reads but check.subject and check.program", () => {
  const check = compiledBlockSchema.types.find(({ name }) => name === "check");
  const heldBack = new Set([check?.attributes.subject, check?.attributes.program]);
  const elements: (BlockElementVersions | undefined)[] = [
    ...(compiledBlockSchema.nodes ?? []),
    ...(compiledBlockSchema.marks ?? []),
    ...compiledBlockSchema.types.flatMap((type) => [
      type,
      ...Object.values(type.attributes)
        .filter((attribute) => !heldBack.has(attribute))
        .flatMap((attribute) => [
          attribute,
          ...(attribute.choices ?? []).map((choice) => attribute.choice_versions?.[choice]),
        ]),
    ]),
  ];
  for (const element of elements) {
    expect(element?.creatable_since ?? 1).toBeLessThanOrEqual(compiledBlockSchema.version);
  }
  expect(schemaCreatable(compiledBlockSchema)).toBe(
    Math.max(...elements.map((element) => element?.since ?? 1))
  );
});

// Decisions, readings and shaped pointers are read from version 7 and created from version 8:
// this build's manifest is version 8, so it creates them and its R is 7, the version they were
// introduced in. The build of version 7, slice 2's, the one that reads them first, reads them
// and creates none: the same manifest at version 7 leaves them out of R, which stands where the
// same manifest without them has it. The check block's `subject` and `program` are read from
// version 6 and created from version 9, one above this build's: this manifest reads them and
// creates neither, so they count toward no R below 9, and the manifest raised to 9 counts them.
test("the compiled manifest reads the new blocks before it creates them, and reads check.subject and check.program without creating them", () => {
  expect(compiledBlockSchema.nodes?.map(({ name }) => name)).toContain("paragraph");
  expect(compiledBlockSchema.types.map(({ name }) => name)).toEqual([
    "callout",
    "ask",
    "requirement",
    "plan_task",
    "check",
    "decision",
    "reading",
  ]);
  expect(compiledBlockSchema.version).toBe(8);
  expect(schemaCreatable(compiledBlockSchema)).toBe(7);
  expect(schemaCreatable({ ...compiledBlockSchema, version: 7 })).toBe(5);
  const version = compiledBlockSchema.version;
  const readsFirst: BlockSchema = { ...compiledBlockSchema, version: version - 1 };
  const ask = compiledBlockSchema.types.find(({ name }) => name === "ask");
  const decision = compiledBlockSchema.types.find(({ name }) => name === "decision");
  const reading = compiledBlockSchema.types.find(({ name }) => name === "reading");
  const shaped = ask?.attributes.shaped;
  if (decision === undefined || reading === undefined || shaped === undefined) {
    throw new Error(
      "the compiled manifest declares the decision and reading blocks and the ask block's shaped"
    );
  }
  for (const element of [decision, reading, shaped]) {
    expect(element.since).toBe(version - 1);
    expect(element.creatable_since).toBe(version);
  }
  const withoutTheNewBlocks = (schema: BlockSchema): BlockSchema => {
    const withoutShaped = withoutAttributes(schema, "ask", ["shaped"]);
    return {
      ...withoutShaped,
      types: withoutShaped.types.filter(({ name }) => name !== "decision" && name !== "reading"),
    };
  };
  expect(schemaCreatable(compiledBlockSchema)).toBe(version - 1);
  expect(schemaCreatable(withoutTheNewBlocks(compiledBlockSchema))).toBeLessThan(version - 1);
  expect(schemaCreatable(readsFirst)).toBe(schemaCreatable(withoutTheNewBlocks(readsFirst)));
  expect(schemaCreatable(readsFirst)).toBeLessThan(version - 1);

  const check = compiledBlockSchema.types.find(({ name }) => name === "check");
  const subject = check?.attributes.subject;
  const program = check?.attributes.program;
  if (check === undefined || subject === undefined || program === undefined) {
    throw new Error("the compiled manifest declares the check block's subject and program");
  }
  for (const attribute of [subject, program]) {
    expect(attribute.since).toBe(version - 2);
    expect(attribute.creatable_since).toBe(version + 1);
  }
  const withoutTheMarks = (schema: BlockSchema): BlockSchema =>
    withoutAttributes(schema, "check", ["subject", "program"]);
  expect(schemaCreatable(compiledBlockSchema)).toBe(
    schemaCreatable(withoutTheMarks(compiledBlockSchema))
  );
  expect(schemaCreatable(readsFirst)).toBe(schemaCreatable(withoutTheMarks(readsFirst)));
  expect(schemaCreatable(readsFirst)).toBeLessThan(version - 2);
  const raised = raisedByOne(compiledBlockSchema);
  expect(schemaCreatable(raised)).toBe(version - 1);
  expect(schemaCreatable(withoutTheMarks(withoutTheNewBlocks(raised)))).toBeLessThan(
    schemaCreatable(withoutTheNewBlocks(raised))
  );
});

test("an element the page creates counts toward its creatable version, one it only reads does not", () => {
  expect(schemaCreatable(withFutureBlock(true))).toBe(compiledBlockSchema.version + 1);
  expect(schemaCreatable(withFutureBlock(false))).toBe(
    schemaCreatable(raisedByOne(compiledBlockSchema))
  );
});

test("nodes, marks, attributes and enum choices each count toward the creatable version", () => {
  const version = compiledBlockSchema.version + 1;
  const raised = schemaCreatable(raisedByOne(compiledBlockSchema));
  const ask = compiledBlockSchema.types.find(({ name }) => name === "ask");
  const urgency = ask?.attributes.urgency;
  if (ask === undefined || urgency === undefined) {
    throw new Error("the compiled manifest declares the ask block's urgency");
  }
  const withAsk = (attributes: BlockSchema["types"][number]["attributes"]): BlockSchema => ({
    ...compiledBlockSchema,
    types: compiledBlockSchema.types.map((type) =>
      type.name === "ask" ? { ...ask, attributes: { ...ask.attributes, ...attributes } } : type
    ),
    version,
  });

  expect(
    schemaCreatable({
      ...compiledBlockSchema,
      nodes: [
        ...(compiledBlockSchema.nodes ?? []),
        { creatable_since: version, name: "aside", since: version },
      ],
      version,
    })
  ).toBe(version);
  expect(
    schemaCreatable({
      ...compiledBlockSchema,
      marks: [
        ...(compiledBlockSchema.marks ?? []),
        { creatable_since: version + 1, name: "glow", since: version },
      ],
      version,
    })
  ).toBe(raised);
  expect(
    schemaCreatable(
      withAsk({ due: { creatable_since: version, kind: "timestamp", since: version } })
    )
  ).toBe(version);
  expect(
    schemaCreatable(
      withAsk({
        urgency: {
          ...urgency,
          choice_versions: { critical: { creatable_since: version, since: version } },
          choices: [...(urgency.choices ?? []), "critical"],
        },
      })
    )
  ).toBe(version);
});
