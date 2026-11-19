import { useCallback, useEffect, useRef, useState } from "react";

import type { Issue } from "../../api/types";
import { useSubmitGuard } from "../../hooks/useSubmitGuard";

export type IssueUpdateInput = Partial<Pick<Issue, "parent" | "route" | "status" | "title">>;

export interface IssueUpdateMutation {
  mutate: (input: IssueUpdateInput) => void;
  variables: IssueUpdateInput | undefined;
}

export interface IssueDrafts {
  discardTitle: () => void;
  discardRoute: () => void;
  discardParent: () => void;
  onIssueSettled: (error: unknown) => void;
  onIssueSuccess: (next: Issue, input: IssueUpdateInput) => void;
  requestParentSubmit: () => boolean;
  requestRouteSubmit: () => boolean;
  requestStatusSubmit: (status: string) => boolean;
  requestTitleSubmit: () => boolean;
  retry: () => void;
  parent: string;
  parentDirty: boolean;
  route: string;
  routeDirty: boolean;
  routeIsValid: boolean;
  title: string;
  titleDirty: boolean;
  titleError: string | null;
  writeParent: (next: string) => void;
  writeRoute: (next: string) => void;
  writeTitle: (next: string) => void;
}

const routePattern = /^(role:[a-z0-9-]+|session:[0-9a-f-]{16,})$/;

export function useIssueDrafts(issue: Issue, updateIssue: IssueUpdateMutation): IssueDrafts {
  const [title, setTitle] = useState(issue.title);
  const [titleDirty, setTitleDirty] = useState(false);
  const [titleError, setTitleError] = useState<string | null>(null);
  const [titlePendingSubmit, setTitlePendingSubmit] = useState<string | null>(null);
  const [route, setRoute] = useState(issue.route ?? "");
  const [routeDirty, setRouteDirty] = useState(false);
  const [routePendingSubmit, setRoutePendingSubmit] = useState<string | null>(null);
  const [parent, setParent] = useState(issue.parent ?? "");
  const [parentDirty, setParentDirty] = useState(false);
  const [parentPendingSubmit, setParentPendingSubmit] = useState<string | null>(null);
  const titleRef = useRef(title);
  const routeRef = useRef(route);
  const parentRef = useRef(parent);
  const titleDirtyRef = useRef(false);
  const routeDirtyRef = useRef(false);
  const parentDirtyRef = useRef(false);
  const titlePendingRef = useRef<string | null>(null);
  const routePendingRef = useRef<string | null>(null);
  const parentPendingRef = useRef<string | null>(null);
  // The value each field's write still out carries, or null. Until that write lands the issue
  // still holds the old value, so a draft is changed, and has something to send, against the
  // value out: a reader who types the old value back is undoing the write.
  const titleOutRef = useRef<string | null>(null);
  const routeOutRef = useRef<string | null>(null);
  const parentOutRef = useRef<string | null>(null);
  const updateIssueGuard = useSubmitGuard();

  const setPendingTitle = (value: string | null) => {
    titlePendingRef.current = value;
    setTitlePendingSubmit(value);
  };
  const setPendingRoute = (value: string | null) => {
    routePendingRef.current = value;
    setRoutePendingSubmit(value);
  };
  const setPendingParent = (value: string | null) => {
    parentPendingRef.current = value;
    setParentPendingSubmit(value);
  };
  const setTitleDraftDirty = (dirty: boolean) => {
    titleDirtyRef.current = dirty;
    setTitleDirty(dirty);
  };
  const setRouteDraftDirty = (dirty: boolean) => {
    routeDirtyRef.current = dirty;
    setRouteDirty(dirty);
  };
  const setParentDraftDirty = (dirty: boolean) => {
    parentDirtyRef.current = dirty;
    setParentDirty(dirty);
  };
  const writeTitle = useCallback(
    (next: string) => {
      titleRef.current = next;
      setTitle(next);
      const dirty = next !== (titleOutRef.current ?? issue.title);
      titleDirtyRef.current = dirty;
      setTitleDirty(dirty);
      setTitleError(null);
    },
    [issue.title]
  );
  const writeRoute = useCallback(
    (next: string) => {
      routeRef.current = next;
      setRoute(next);
      const dirty = next !== (routeOutRef.current ?? issue.route ?? "");
      routeDirtyRef.current = dirty;
      setRouteDirty(dirty);
    },
    [issue.route]
  );
  const writeParent = useCallback(
    (next: string) => {
      parentRef.current = next;
      setParent(next);
      const dirty = next !== (parentOutRef.current ?? issue.parent ?? "");
      parentDirtyRef.current = dirty;
      setParentDirty(dirty);
    },
    [issue.parent]
  );
  const applyServerTitle = useCallback((next: string) => {
    titleRef.current = next;
    setTitle(next);
  }, []);
  const applyServerRoute = useCallback((next: string) => {
    routeRef.current = next;
    setRoute(next);
  }, []);
  const applyServerParent = useCallback((next: string) => {
    parentRef.current = next;
    setParent(next);
  }, []);

  // Each value is marked out inside the guard, as its write goes: a write the guard holds back
  // is not out, and stays a draft to send.
  function drainPendingSubmits(): void {
    const pendingTitle = titlePendingRef.current;
    if (pendingTitle !== null) {
      updateIssueGuard.guard(() => {
        titleOutRef.current = pendingTitle;
        updateIssue.mutate({ title: pendingTitle });
      });
      return;
    }
    const pendingRoute = routePendingRef.current;
    if (pendingRoute !== null) {
      updateIssueGuard.guard(() => {
        routeOutRef.current = pendingRoute;
        updateIssue.mutate({ route: pendingRoute });
      });
      return;
    }
    const pendingParent = parentPendingRef.current;
    if (pendingParent !== null) {
      // An empty draft clears the parent: the wire form is null, never "".
      updateIssueGuard.guard(() => {
        parentOutRef.current = pendingParent;
        updateIssue.mutate({ parent: pendingParent === "" ? null : pendingParent });
      });
    }
  }

  function requestTitleSubmit(): boolean {
    const submitted = titleRef.current.trim();
    if (issue.closed_at !== null || submitted === "") {
      setPendingTitle(null);
      setTitleError("Title is required.");
      return false;
    }
    setTitleError(null);
    if (submitted === (titleOutRef.current ?? issue.title)) {
      applyServerTitle(submitted);
      setTitleDraftDirty(false);
      // A draft that is the title of the write still out stays pending with that write until it
      // lands. With nothing pending the issue's old title is put back over it while the write is
      // out, and after a refusal Retry has only that old title, which sends nothing.
      setPendingTitle(titleOutRef.current);
      return false;
    }
    setPendingTitle(submitted);
    drainPendingSubmits();
    return true;
  }

  function requestRouteSubmit(): boolean {
    const submitted = routeRef.current;
    if (issue.closed_at !== null || (submitted !== "" && !routePattern.test(submitted))) {
      setPendingRoute(null);
      return false;
    }
    if (submitted === (routeOutRef.current ?? issue.route ?? "")) {
      setRouteDraftDirty(false);
      // As the title's: a draft that is the route of the write still out stays pending with it.
      setPendingRoute(routeOutRef.current);
      return false;
    }
    setPendingRoute(submitted);
    drainPendingSubmits();
    return true;
  }

  function requestParentSubmit(): boolean {
    const submitted = parentRef.current.trim();
    if (issue.closed_at !== null) {
      setPendingParent(null);
      return false;
    }
    if (submitted === (parentOutRef.current ?? issue.parent ?? "")) {
      applyServerParent(submitted);
      setParentDraftDirty(false);
      // As the title's: a draft that is the parent of the write still out stays pending with it.
      setPendingParent(parentOutRef.current);
      return false;
    }
    setPendingParent(submitted);
    drainPendingSubmits();
    return true;
  }

  const onIssueSuccess = (next: Issue, input: IssueUpdateInput) => {
    if (input.title !== undefined && titlePendingRef.current === input.title) {
      setPendingTitle(null);
    }
    if (input.route !== undefined && routePendingRef.current === input.route) {
      setPendingRoute(null);
    }
    if (input.parent !== undefined && parentPendingRef.current === (input.parent ?? "")) {
      setPendingParent(null);
    }
    // A draft that is not changed takes the value the write lands with, a draft a discard put
    // back while the write was out too: it is the issue's value, and marked changed it would send
    // the old value over the new one. Another field's write can land while this field's write
    // waits behind it in the issue's line (a title saved after a status pick), with the value
    // that write is about to replace: the draft is judged against the value out then, as it is
    // while the reader types.
    const titleBase = (input.title === undefined ? titleOutRef.current : null) ?? next.title;
    const routeBase = (input.route === undefined ? routeOutRef.current : null) ?? next.route ?? "";
    const parentBase =
      (input.parent === undefined ? parentOutRef.current : null) ?? next.parent ?? "";
    if (!titleDirtyRef.current || titleRef.current.trim() === titleBase) {
      applyServerTitle(titleBase);
    }
    if (!routeDirtyRef.current) {
      applyServerRoute(routeBase);
    }
    if (!parentDirtyRef.current) {
      applyServerParent(parentBase);
    }
    setTitleDraftDirty(titleRef.current.trim() !== titleBase);
    setRouteDraftDirty(routeRef.current !== routeBase);
    setParentDraftDirty(parentRef.current.trim() !== parentBase);
  };
  const onIssueSettled = (error: unknown) => {
    // A write that ended without landing left the issue's value as it was, so a draft marked
    // against that write's value is marked against the issue's again. A draft that was the value
    // out is then changed: left marked not changed, the next landing of another field's write
    // would put the issue's value back over it.
    if (error !== null) {
      if (titleOutRef.current !== null) {
        setTitleDraftDirty(titleRef.current !== issue.title);
      }
      if (routeOutRef.current !== null) {
        setRouteDraftDirty(routeRef.current !== (issue.route ?? ""));
      }
      if (parentOutRef.current !== null) {
        setParentDraftDirty(parentRef.current !== (issue.parent ?? ""));
      }
    }
    // The write is over, landed or not: a draft is changed against the issue's value again.
    titleOutRef.current = null;
    routeOutRef.current = null;
    parentOutRef.current = null;
    updateIssueGuard.release();
    if (error === null) {
      drainPendingSubmits();
    }
  };
  const retry = () => {
    const { variables } = updateIssue;
    if (variables?.title !== undefined) {
      requestTitleSubmit();
      drainPendingSubmits();
      return;
    }
    if (variables?.route !== undefined) {
      requestRouteSubmit();
      drainPendingSubmits();
      return;
    }
    if (variables?.parent !== undefined) {
      requestParentSubmit();
      drainPendingSubmits();
      return;
    }
    updateIssueGuard.retryLast(updateIssue);
  };
  const discardTitle = () => {
    applyServerTitle(issue.title);
    setTitleDraftDirty(false);
    setTitleError(null);
    setPendingTitle(null);
  };
  const discardRoute = () => {
    applyServerRoute(issue.route ?? "");
    setRouteDraftDirty(false);
    setPendingRoute(null);
  };
  const discardParent = () => {
    applyServerParent(issue.parent ?? "");
    setParentDraftDirty(false);
    setPendingParent(null);
  };
  const requestStatusSubmit = (status: string): boolean =>
    updateIssueGuard.guard(() => updateIssue.mutate({ status }));

  useEffect(() => {
    if (!titleDirty && !titleDirtyRef.current && titlePendingSubmit === null) {
      applyServerTitle(issue.title);
    }
  }, [applyServerTitle, issue.title, titleDirty, titlePendingSubmit]);
  useEffect(() => {
    if (!routeDirty && !routeDirtyRef.current && routePendingSubmit === null) {
      applyServerRoute(issue.route ?? "");
    }
  }, [applyServerRoute, issue.route, routeDirty, routePendingSubmit]);
  useEffect(() => {
    if (!parentDirty && !parentDirtyRef.current && parentPendingSubmit === null) {
      applyServerParent(issue.parent ?? "");
    }
  }, [applyServerParent, issue.parent, parentDirty, parentPendingSubmit]);

  return {
    discardTitle,
    discardRoute,
    discardParent,
    onIssueSettled,
    onIssueSuccess,
    requestParentSubmit,
    requestRouteSubmit,
    requestTitleSubmit,
    retry,
    parent,
    parentDirty,
    route,
    routeDirty,
    routeIsValid: route === "" || routePattern.test(route),
    title,
    requestStatusSubmit,
    titleDirty,
    titleError,
    writeParent,
    writeRoute,
    writeTitle,
  };
}
