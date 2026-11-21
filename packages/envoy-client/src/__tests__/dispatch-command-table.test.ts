import { expect, test } from "bun:test";
import { dispatchToolSpecs } from "@legion/contracts";
import {
  type DispatchCommand,
  dispatchCommandOf,
  dispatchCommands,
  type HandWrittenToolName,
} from "../dispatch-commands/table";

// The whole point of typing the table `Record<HandWrittenToolName, DispatchCommand>`: a command
// the contract names with no route binding and the table does not hold is refused by the type
// checker, never found at run time by the host that called it. The deletion that proves it cannot
// live in the tree, so the table is spread here with one command taken out of it and the hole
// handed to the same type. `bun run typecheck` fails on this file the day that stops being an
// error.
const { dispatch_read: _read, ...withoutRead } = dispatchCommands;

// @ts-expect-error - a table that does not hold every hand-written command is refused.
const missingOneCommand: Record<HandWrittenToolName, DispatchCommand> = withoutRead;

const boundSpecs = dispatchToolSpecs.filter((spec) => "routes" in spec);
const handWrittenSpecs = dispatchToolSpecs.filter((spec) => !("routes" in spec));

test("the table holds one handler for every command the contract names with no route binding", () => {
  expect(Object.keys(dispatchCommands).sort()).toEqual(
    handWrittenSpecs.map((spec) => spec.name).sort()
  );
});

test("a table missing one command holds one fewer handler, and no type", () => {
  expect(Object.keys(missingOneCommand)).toHaveLength(handWrittenSpecs.length - 1);
});

test("every command the contract names has a command, a bound spec the generic one", () => {
  expect(boundSpecs.length).toBeGreaterThan(0);
  for (const spec of dispatchToolSpecs) {
    const command = dispatchCommandOf(spec.name);
    expect(command, spec.name).toBeDefined();
    if ("routes" in spec) {
      expect(command?.origin, spec.name).toBe("resolved");
    } else {
      expect(command, spec.name).toBe(dispatchCommands[spec.name]);
    }
  }
  expect(dispatchCommandOf("dispatch_paint_widget")).toBeUndefined();
});
