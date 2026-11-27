import { Component, type ComponentProps, type ErrorInfo, type ReactNode } from "react";

import {
  calloutDangerBorder,
  dangerText,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/shell-classes";

interface ErrorBoundaryProps {
  children: ReactNode;
  /** A page identity: navigating to a different page clears an error from the old one. */
  resetKey?: string;
  /** What stopped rendering, named for the reader: `this page`, `the margin`, `Dispatch`. */
  region: string;
}

interface ErrorBoundaryState {
  message: string | undefined;
}

/** The class of an action under an error's heading: the reload (`ReloadButton`), and any other
 *  control a caller adds beside it. */
export const ERROR_ACTION_CLASS = `mt-3 text-sm font-medium underline ${dangerText}`;

/** The button that reloads the tab, labelled `children`: the last control of an error drawn in
 *  a part's place (`ErrorView`), and of the line that stands in the pending-writes indicator's
 *  place while its chunk cannot load (`ChangesNotSent`, `features/writes/write-hosts.tsx`). One
 *  piece, so a change to how the tab reloads reaches both. */
export function ReloadButton({ children }: { children: ReactNode }): ReactNode {
  return (
    <button className={ERROR_ACTION_CLASS} onClick={() => window.location.reload()} type="button">
      {children}
    </button>
  );
}

/**
 * How the reader is told that a part of Dispatch failed, in that part's place: a region that
 * stopped rendering (`ErrorBoundary`) and a surface that could not open (`askedSurface`). One
 * piece, so the two look the same: the heading that says what failed, what the caller adds under
 * it, and last a button that reloads the tab, labelled `reload` (`ReloadButton`). The caller
 * gives the element its role, and its place on the page (`className`).
 */
export function ErrorView({
  children,
  className = "",
  heading,
  reload,
  ...section
}: ComponentProps<"section"> & { heading: string; reload: string }): ReactNode {
  return (
    <section {...section} className={`rounded-xl border p-4 ${calloutDangerBorder} ${className}`}>
      <h2 className={`text-base font-semibold ${textPrimaryOnCanvas}`}>{heading}</h2>
      {children}
      <ReloadButton>{reload}</ReloadButton>
    </section>
  );
}

/**
 * React unmounts the whole tree when a render throws and nothing catches it, which is a blank
 * page — no sidebar, no error, nothing to act on. This catches the throw where it happened, so
 * the rest of the shell stays usable and the reader is told which region failed and why.
 */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { message: undefined };

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { message: error instanceof Error ? error.message : String(error) };
  }

  componentDidCatch(error: unknown, info: ErrorInfo): void {
    console.error(`Dispatch: ${this.props.region} failed to render`, error, info.componentStack);
  }

  componentDidUpdate(previous: ErrorBoundaryProps): void {
    if (previous.resetKey !== this.props.resetKey && this.state.message !== undefined) {
      this.setState({ message: undefined });
    }
  }

  render(): ReactNode {
    const { message } = this.state;
    if (message === undefined) {
      return this.props.children;
    }
    return (
      <ErrorView
        data-testid="error-boundary"
        heading={`Something went wrong in ${this.props.region}.`}
        reload="Reload Dispatch"
        role="alert"
      >
        <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
          The rest of Dispatch still works. Reloading usually clears it; if it comes back, the
          message below names the failure.
        </p>
        <p className={`mt-2 font-mono text-xs break-words ${dangerText}`}>{message}</p>
      </ErrorView>
    );
  }
}
