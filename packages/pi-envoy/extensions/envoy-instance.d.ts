// The extension under a query string: `import("./envoy.ts?<row>")` in this directory's tests. Bun
// loads each query as a module of its own, so each row's instance keeps its own state; tsc reads
// no query string, so this pattern types every such instance. A row drives an instance on the
// stand-in host (`../test/stand-in-host.ts`), which carries the part of Oh My Pi's API the
// extension uses, so that is the host an instance takes here; `extensions/envoy.ts`'s own type is
// checked against the full API where it is compiled itself. The tests' seams are the module's own.
declare module "./envoy.ts?*" {
  const extension: (pi: import("../test/stand-in-host").TestPi) => void;
  export default extension;
  export const setNatsConnectForTests: typeof import("./envoy").setNatsConnectForTests;
  export const resetNatsConnectForTests: typeof import("./envoy").resetNatsConnectForTests;
  export const resetSessionRoleLoggedForTests: typeof import("./envoy").resetSessionRoleLoggedForTests;
}
