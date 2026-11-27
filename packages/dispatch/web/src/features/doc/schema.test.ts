import { expect, test } from "bun:test";

import type { BlockSchema } from "../../api/types";
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

// The page presents the manifest it was built with: its version V, and R, the highest `since`
// among the elements it may create (`creatable_since` V or less), as Go's `pmdoc.SchemaCreatable`
// computes it. The manifest stages the task block's `parent`: it reads it from its version and
// creates it only from the next. So it leaves R where the same manifest without it has it, below
// V, and it counts toward R in the manifest at the next version. The numbers come from the
// manifest, never from a literal here.
test("the compiled manifest is the page's own, and it reads plan_task.parent without creating it", () => {
  expect(compiledBlockSchema.nodes?.map(({ name }) => name)).toContain("paragraph");
  expect(compiledBlockSchema.types.map(({ name }) => name)).toEqual([
    "callout",
    "ask",
    "requirement",
    "plan_task",
    "check",
  ]);
  const version = compiledBlockSchema.version;
  const task = compiledBlockSchema.types.find(({ name }) => name === "plan_task");
  const parent = task?.attributes.parent;
  if (task === undefined || parent === undefined) {
    throw new Error("the compiled manifest declares the plan_task block's parent");
  }
  expect(parent).toMatchObject({ creatable_since: version + 1, kind: "block[]", since: version });
  const withoutIt = (schema: BlockSchema): BlockSchema => ({
    ...schema,
    types: schema.types.map((type) =>
      type.name === "plan_task"
        ? {
            ...type,
            attributes: Object.fromEntries(
              Object.entries(type.attributes).filter(([name]) => name !== "parent")
            ),
          }
        : type
    ),
  });

  expect(schemaCreatable(compiledBlockSchema)).toBe(
    schemaCreatable(withoutIt(compiledBlockSchema))
  );
  expect(schemaCreatable(compiledBlockSchema)).toBeLessThan(version);
  expect(schemaCreatable(raisedByOne(compiledBlockSchema))).toBe(version);
  expect(schemaCreatable(withoutIt(raisedByOne(compiledBlockSchema)))).toBeLessThan(version);
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
