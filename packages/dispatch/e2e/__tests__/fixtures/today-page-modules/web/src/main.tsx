import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { App } from "./app";
import type { Shape } from "./shapes";
import "./styles.css";

const shape: Shape = { name: "circle" };

const parser = new Worker(new URL("./parse.worker.ts", import.meta.url), { type: "module" });
parser.postMessage(shape.name);

createRoot(document.body).render(
  <StrictMode>
    <App shape={shape} />
  </StrictMode>
);
