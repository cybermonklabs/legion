import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import { CacheProvider, createAppCache } from "./api/cache/provider";
import { App } from "./app";
import {
  DeploymentResilience,
  installChunkFailureRecovery,
} from "./features/shell/DeploymentResilience";
import { ErrorBoundary } from "./features/shell/ErrorBoundary";
import "./styles.css";
const cache = createAppCache();

const root = document.getElementById("root");
if (root === null) {
  throw new Error("Dispatch could not find its root element.");
}

installChunkFailureRecovery();

createRoot(root).render(
  <StrictMode>
    <CacheProvider cache={cache}>
      <BrowserRouter>
        <DeploymentResilience />
        <ErrorBoundary region="Dispatch">
          <App />
        </ErrorBoundary>
      </BrowserRouter>
    </CacheProvider>
  </StrictMode>
);
