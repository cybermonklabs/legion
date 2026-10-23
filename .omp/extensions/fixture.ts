import { mkdirSync, writeFileSync } from "node:fs";
export default function fixtureExtension() {
  mkdirSync("/tmp/legion-fixture", { recursive: true });
  writeFileSync("/tmp/legion-fixture/omp-extension", "loaded\n");
}
