import { lazy, type ReactNode, Suspense } from "react";

// biome-ignore lint/style/useImportType: the walk's case of an import whose every name is marked `type`, the form of an import that loads nothing other than `import type`
import { type Shape, type ShapeName } from "./shapes";

export { IssuePage } from "./features/issue-page/IssuePage";

const Dashboard = lazy(() => import("@fixture/editor/dashboard"));

export function App({ shape }: { shape: Shape }): ReactNode {
  const name: ShapeName = shape.name;
  return (
    <Suspense>
      <Dashboard name={name} />
    </Suspense>
  );
}
