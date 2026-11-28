import { expect, test } from "bun:test";

import type { BlockElementVersions, BlockSchema } from "../../api/types";
import { compiledBlockSchema, editorBlockSchema } from "./schema";
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

/** The manifest without the elements read from version `from` or later, as the build before that
 *  version declares it: from 9, the issue tree's criterion block, the attributes the tree adds to
 *  the requirement, the plan task and the check, and the issue item of a plan task's `needs`; from
 *  10, the component and conclusion blocks and the change request's attributes too. */
function withoutElementsSince(schema: BlockSchema, from: number): BlockSchema {
  const older = (element: BlockElementVersions | undefined) => (element?.since ?? 1) < from;
  return {
    ...schema,
    types: schema.types.filter(older).map((type) => ({
      ...type,
      attributes: Object.fromEntries(
        Object.entries(type.attributes)
          .filter(([, attribute]) => older(attribute))
          .map(
            ([name, attribute]) =>
              [
                name,
                attribute.items === undefined
                  ? attribute
                  : {
                      ...attribute,
                      items: attribute.items.filter((item) =>
                        older(attribute.item_versions?.[item])
                      ),
                    },
              ] as const
          )
      ),
    })),
  };
}

// The page presents the manifest it was built with: its version V, and R, the highest `since`
// among the elements it may create (`creatable_since` V or less), as Go's `pmdoc.SchemaCreatable`
// computes it. Today's manifest creates every element it reads, and its R is the highest `since`
// of them all, whatever that is: the numbers come from the manifest, never from a literal here,
// so raising the manifest does not break this row.
test("the compiled manifest is the page's own, and it creates everything it reads", () => {
  const elements: (BlockElementVersions | undefined)[] = [
    ...(compiledBlockSchema.nodes ?? []),
    ...(compiledBlockSchema.marks ?? []),
    ...compiledBlockSchema.types.flatMap((type) => [
      type,
      ...Object.values(type.attributes).flatMap((attribute) => [
        attribute,
        ...(attribute.choices ?? []).map((choice) => attribute.choice_versions?.[choice]),
        ...(attribute.items ?? []).map((item) => attribute.item_versions?.[item]),
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

// Decisions, readings and shaped pointers are read from version 7 and created from version 8;
// the issue tree's criterion block, its `parent`, `serves`, `size`, `skills` and `docs`
// attributes and the issue item kind of a plan task's `needs` are read and created from version
// 9; the component and conclusion blocks and the change request's attributes are read and created
// from version 10. This build's manifest is version 10, so it creates all of them and its R is 10.
// The build of version 9 creates the issue tree's elements and none of version 10's: the same
// manifest at version 9 has R 9. The build of version 8 creates decisions, readings and shaped
// pointers and none of version 9's elements: the same manifest at version 8 has R 7, the version
// they were introduced in. The build of version 7, slice 2's, the one that reads them first, reads
// them and creates none: the same manifest at version 7 leaves them out of R, which stands where
// the same manifest without them has it, and at version 6 R is 5 as well. The check block's
// `subject` and `program` are read from version 6 and created from version 10, this build's: the
// builds from 6 to 9 read them and create neither, so they count toward no R below 10, and this
// manifest counts them.
test("the compiled manifest reads the new blocks before it creates them, and reads check.subject and check.program before it creates them", () => {
  expect(compiledBlockSchema.nodes?.map(({ name }) => name)).toContain("paragraph");
  expect(compiledBlockSchema.types.map(({ name }) => name)).toEqual([
    "callout",
    "ask",
    "requirement",
    "plan_task",
    "check",
    "decision",
    "reading",
    "criterion",
    "component",
    "conclusion",
  ]);
  expect(compiledBlockSchema.version).toBe(10);
  expect(schemaCreatable(compiledBlockSchema)).toBe(10);
  expect(schemaCreatable({ ...compiledBlockSchema, version: 9 })).toBe(9);
  expect(schemaCreatable({ ...compiledBlockSchema, version: 8 })).toBe(7);
  expect(schemaCreatable({ ...compiledBlockSchema, version: 7 })).toBe(5);
  expect(schemaCreatable({ ...compiledBlockSchema, version: 6 })).toBe(5);
  const version = compiledBlockSchema.version;
  const createsFirst: BlockSchema = { ...compiledBlockSchema, version: 8 };
  const readsFirst: BlockSchema = { ...compiledBlockSchema, version: 7 };
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
    expect(element.since).toBe(readsFirst.version);
    expect(element.creatable_since).toBe(createsFirst.version);
  }
  const withoutTheNewBlocks = (schema: BlockSchema): BlockSchema => {
    const withoutShaped = withoutAttributes(schema, "ask", ["shaped"]);
    return {
      ...withoutShaped,
      types: withoutShaped.types.filter(({ name }) => name !== "decision" && name !== "reading"),
    };
  };
  expect(schemaCreatable(createsFirst)).toBe(readsFirst.version);
  expect(schemaCreatable(withoutTheNewBlocks(createsFirst))).toBeLessThan(readsFirst.version);
  expect(schemaCreatable(readsFirst)).toBe(schemaCreatable(withoutTheNewBlocks(readsFirst)));
  expect(schemaCreatable(readsFirst)).toBeLessThan(readsFirst.version);

  const check = compiledBlockSchema.types.find(({ name }) => name === "check");
  const subject = check?.attributes.subject;
  const program = check?.attributes.program;
  if (check === undefined || subject === undefined || program === undefined) {
    throw new Error("the compiled manifest declares the check block's subject and program");
  }
  for (const attribute of [subject, program]) {
    expect(attribute.since).toBe(6);
    expect(attribute.creatable_since).toBe(version);
  }
  const withoutTheMarks = (schema: BlockSchema): BlockSchema =>
    withoutAttributes(schema, "check", ["subject", "program"]);
  const beforeTheMarks: BlockSchema = { ...compiledBlockSchema, version: version - 1 };
  expect(schemaCreatable(beforeTheMarks)).toBe(schemaCreatable(withoutTheMarks(beforeTheMarks)));
  expect(schemaCreatable(readsFirst)).toBe(schemaCreatable(withoutTheMarks(readsFirst)));
  expect(schemaCreatable(readsFirst)).toBeLessThan(6);
  // Only once every element read from a later version than the marks is gone does R show them,
  // and only at the version that creates them.
  const beforeTheNewBlocks = withoutTheNewBlocks(withoutElementsSince(compiledBlockSchema, 9));
  expect(schemaCreatable(withoutTheMarks(beforeTheNewBlocks))).toBeLessThan(
    schemaCreatable(beforeTheNewBlocks)
  );
  const markedBeforeTheMarks = { ...beforeTheNewBlocks, version: version - 1 };
  expect(schemaCreatable(withoutTheMarks(markedBeforeTheMarks))).toBe(
    schemaCreatable(markedBeforeTheMarks)
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

// A document of an issue whose tree read answers `view_of` is edited under a schema in which a
// requirement may not be created, by the type's own `creatable_since`, and nothing else differs;
// every other document is edited under the compiled manifest itself.
test("a view's document gets a schema that may not create a requirement, every other document the compiled one", () => {
  const view = editorBlockSchema("CORE-1");
  const requirement = view.types.find(({ name }) => name === "requirement");
  const compiled = compiledBlockSchema.types.find(({ name }) => name === "requirement");
  expect(requirement).toBeDefined();
  expect(requirement?.creatable_since ?? 1).toBeGreaterThan(view.version);
  expect(compiled?.creatable_since ?? 1).toBeLessThanOrEqual(compiledBlockSchema.version);
  expect({ ...requirement, creatable_since: undefined }).toEqual({
    ...compiled,
    creatable_since: undefined,
  });
  expect(view.types.filter(({ name }) => name !== "requirement")).toEqual(
    compiledBlockSchema.types.filter(({ name }) => name !== "requirement")
  );
  expect({ ...view, types: [] }).toEqual({ ...compiledBlockSchema, types: [] });
  for (const viewOf of [null, undefined, ""]) {
    expect(editorBlockSchema(viewOf)).toBe(compiledBlockSchema);
  }
});
