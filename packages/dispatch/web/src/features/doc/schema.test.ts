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

// The page presents the manifest it was built with: its version V, and R, the highest `since`
// among the elements it may create (`creatable_since` V or less), as Go's `pmdoc.SchemaCreatable`
// computes it. Today's manifest creates every element it reads, the newest of them included, so
// its R is the highest `since` it declares, whatever that is: the numbers come from the manifest,
// never from a literal here, so raising the manifest does not break this row.
test("the compiled manifest is the page's own, and it creates everything it reads", () => {
  const elements: (BlockElementVersions | undefined)[] = [
    ...(compiledBlockSchema.nodes ?? []),
    ...(compiledBlockSchema.marks ?? []),
    ...compiledBlockSchema.types.flatMap((type) => [
      type,
      ...Object.values(type.attributes).flatMap((attribute) => [
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

// Decisions, readings and shaped pointers are read from version 6 and created from version 7:
// this build's manifest is version 7, so it creates them and its R is 6, the version they were
// introduced in. A build of version 6, the one that reads them first, reads them and creates
// none: the same manifest at version 6 has R 5, the last version whose elements that build may
// introduce.
test("the compiled manifest reads the new blocks before it creates them", () => {
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
  expect(compiledBlockSchema.version).toBe(7);
  expect(schemaCreatable(compiledBlockSchema)).toBe(6);
  expect(schemaCreatable({ ...compiledBlockSchema, version: 6 })).toBe(5);
});

test("an element the page creates counts toward its creatable version, one it only reads does not", () => {
  const creatableAtNextVersion = schemaCreatable({
    ...compiledBlockSchema,
    version: compiledBlockSchema.version + 1,
  });
  expect(schemaCreatable(withFutureBlock(true))).toBe(compiledBlockSchema.version + 1);
  expect(schemaCreatable(withFutureBlock(false))).toBe(creatableAtNextVersion);
});

test("nodes, marks, attributes and enum choices each count toward the creatable version", () => {
  const version = compiledBlockSchema.version + 1;
  const creatableAtNextVersion = schemaCreatable({ ...compiledBlockSchema, version });
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
  ).toBe(creatableAtNextVersion);
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
