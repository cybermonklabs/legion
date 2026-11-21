/** The comparison of the ask card's pictures (INFRA-3231's check b5ee93c7): each picture the
 *  build drew beside the demo's picture of the same screen, and the share of its pixels that
 *  changed. `ask-card-pictures.e2e.ts` compares as it takes the pictures where it is given the
 *  demo's, and `e2e/tools/ask-card-pictures-diff.ts` compares a directory of pictures taken
 *  elsewhere, through the same function.
 *
 *  The comparison is pixelmatch's, at its default threshold of 0.1, run as the pinned package's
 *  own command (`bunx pixelmatch@5.3.0`): the suite holds no image decoder, and this is the
 *  one comparison a reader of the share can look up. Its answer counts the pixels whose colours
 *  differ past the threshold, leaving out anti-aliased edges, and writes a picture of where they
 *  are. */
import { spawnSync } from "node:child_process";
import { copyFileSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";

/** The pixelmatch release the comparison runs. */
const pixelmatch = "pixelmatch@5.3.0";

/** One picture beside the demo's. */
export interface PictureComparison {
  readonly name: string;
  readonly width: number;
  readonly height: number;
  /** How many pixels differ, and their share of the picture, from 0 to 1. A picture whose size
   *  is not the demo's differs everywhere, and says so (`demoWidth`, `demoHeight`). */
  readonly changed: number;
  readonly share: number;
  /** The demo's picture's size, which differs from this one's where `sizeMismatch` is true: a
   *  viewport or a device pixel ratio other than the demo's, not a change of what is drawn. */
  readonly demoWidth: number;
  readonly demoHeight: number;
  readonly sizeMismatch: boolean;
}

/** The width and height a PNG's header gives. */
function sizeOf(file: string): { width: number; height: number } {
  const header = readFileSync(file).subarray(16, 24);
  return { height: header.readUInt32BE(4), width: header.readUInt32BE(0) };
}

/** Compares `built` with `demo`, writing where they differ to `diff`. */
export function comparePicture(
  name: string,
  built: string,
  demo: string,
  diff: string
): PictureComparison {
  const { width, height } = sizeOf(built);
  const demoSize = sizeOf(demo);
  const sizes = { demoHeight: demoSize.height, demoWidth: demoSize.width };
  if (demoSize.width !== width || demoSize.height !== height) {
    return {
      ...sizes,
      changed: width * height,
      height,
      name,
      share: 1,
      sizeMismatch: true,
      width,
    };
  }
  const run = spawnSync("bunx", [pixelmatch, built, demo, diff, "0.1"], { encoding: "utf8" });
  // pixelmatch exits 0 where nothing differs and 66 where something does; anything else is no
  // comparison at all.
  if (run.status !== 0 && run.status !== 66) {
    throw new Error(
      `${pixelmatch} could not compare ${name} (exit ${run.status}): ${run.stderr}${run.stdout}`
    );
  }
  const changed = Number(/different pixels: (\d+)/.exec(run.stdout)?.[1]);
  if (!Number.isFinite(changed)) {
    throw new Error(
      `${pixelmatch} gave no count of the pixels of ${name} that differ: ${run.stdout}`
    );
  }
  return {
    ...sizes,
    changed,
    height,
    name,
    share: changed / (width * height),
    sizeMismatch: false,
    width,
  };
}

/** Compares every picture named in `names` in `builtDir` with the demo's of the same name in
 *  `demoDir`, and writes into `outDir`, for each, the build's picture, the demo's and their
 *  difference (`<name>.png`, `<name>.demo.png`, `<name>.diff.png`), then `report.json` and
 *  `report.md`, which list each one's share of changed pixels. */
export function comparePictures(
  names: readonly string[],
  builtDir: string,
  demoDir: string,
  outDir: string
): PictureComparison[] {
  mkdirSync(outDir, { recursive: true });
  const comparisons = names.map((name) => {
    const built = join(outDir, `${name}.png`);
    if (join(builtDir, `${name}.png`) !== built) copyFileSync(join(builtDir, `${name}.png`), built);
    const demo = join(outDir, `${name}.demo.png`);
    copyFileSync(join(demoDir, `${name}.png`), demo);
    return comparePicture(name, built, demo, join(outDir, `${name}.diff.png`));
  });
  writeFileSync(join(outDir, "report.json"), `${JSON.stringify(comparisons, null, 2)}\n`);
  writeFileSync(
    join(outDir, "report.md"),
    [
      "| Picture | Changed pixels | Share | Note |",
      "| --- | ---: | ---: | --- |",
      ...comparisons.map(
        (one) =>
          `| ${one.name} | ${one.changed} | ${(one.share * 100).toFixed(2)}% | ${
            one.sizeMismatch
              ? `size ${one.width}x${one.height}, the demo's ${one.demoWidth}x${one.demoHeight}: not compared`
              : ""
          } |`
      ),
      "",
    ].join("\n")
  );
  return comparisons;
}
