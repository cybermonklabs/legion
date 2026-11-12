import { useCallback, useEffect, useRef, useState } from "react";

import type { Issue } from "../../api/types";

export type IssueUpdateInput = Partial<Pick<Issue, "parent" | "route" | "status" | "title">>;

export interface IssueUpdateMutation {
  mutate: (input: IssueUpdateInput) => void;
  /** One save, awaited: the header reports the refusal of the save that failed, which the
   *  mutation's own state cannot name once a later save of another field was pressed. */
  mutateAsync: (input: IssueUpdateInput) => Promise<unknown>;
  variables: IssueUpdateInput | undefined;
}

/** The save the header reports as refused: what it sent, and why Dispatch refused it. Each press
 *  is a write of its own in the issue's line, so the newest press is not always the one that
 *  failed. */
export interface RefusedSave {
  readonly input: IssueUpdateInput;
  readonly error: Error;
}

export interface IssueDrafts {
  discardTitle: () => void;
  discardRoute: () => void;
  discardParent: () => void;
  onIssueSuccess: (next: Issue) => void;
  /** The last save Dispatch refused, until the next save of the header is pressed. */
  refusal: RefusedSave | undefined;
  /** Whether the parent editor may close: a save was pressed. */
  requestParentSubmit: () => boolean;
  /** Whether the route editor may close: a save was pressed. */
  requestRouteSubmit: () => boolean;
  requestStatusSubmit: (status: string) => void;
  requestTitleSubmit: () => void;
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

/**
 * The drafts of the issue header's four writes - the title, the route, the parent and the status -
 * and what each save sends.
 *
 * Every save joins the store's line when it is pressed (`updateIssue`, one write of the issue's
 * own line, `issue:<KEY>`), as a field pick does: the store orders them, so a title typed while
 * an earlier save is out reaches Dispatch after it, whatever becomes of the header - another
 * page, an unmount, a reload - and none the person pressed is lost. The hook holds no pending
 * save of its own and no submit guard (R8 of dispatch://INFRA-299): what it keeps is the draft
 * the person is typing, which the server's answer replaces only once nothing of theirs is
 * unsaved.
 */
export function useIssueDrafts(issue: Issue, updateIssue: IssueUpdateMutation): IssueDrafts {
  const [title, setTitle] = useState(issue.title);
  const [titleDirty, setTitleDirty] = useState(false);
  const [titleError, setTitleError] = useState<string | null>(null);
  const [route, setRoute] = useState(issue.route ?? "");
  const [routeDirty, setRouteDirty] = useState(false);
  const [parent, setParent] = useState(issue.parent ?? "");
  const [parentDirty, setParentDirty] = useState(false);
  const titleRef = useRef(title);
  const routeRef = useRef(route);
  const parentRef = useRef(parent);
  const titleDirtyRef = useRef(false);
  const routeDirtyRef = useRef(false);
  const parentDirtyRef = useRef(false);
  const [refusal, setRefusal] = useState<RefusedSave | undefined>(undefined);

  /** One save, joined to the issue's line when it is pressed. The press that fails is the one
   *  reported, whatever the header pressed after it: a later save that lands clears its own
   *  refusal and no other's. */
  const submit = (input: IssueUpdateInput): void => {
    setRefusal(undefined);
    void updateIssue.mutateAsync(input).then(
      () => setRefusal((current) => (current?.input === input ? undefined : current)),
      (error: unknown) =>
        setRefusal({ error: error instanceof Error ? error : new Error(String(error)), input })
    );
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
      const dirty = next !== issue.title;
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
      const dirty = next !== (issue.route ?? "");
      routeDirtyRef.current = dirty;
      setRouteDirty(dirty);
    },
    [issue.route]
  );
  const writeParent = useCallback(
    (next: string) => {
      parentRef.current = next;
      setParent(next);
      const dirty = next !== (issue.parent ?? "");
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

  function requestTitleSubmit(): void {
    const submitted = titleRef.current.trim();
    if (issue.closed_at !== null || submitted === "") {
      setTitleError("Title is required.");
      return;
    }
    setTitleError(null);
    if (submitted === issue.title) {
      applyServerTitle(submitted);
      setTitleDraftDirty(false);
      return;
    }
    submit({ title: submitted });
  }

  function requestRouteSubmit(): boolean {
    const submitted = routeRef.current;
    if (issue.closed_at !== null || (submitted !== "" && !routePattern.test(submitted))) {
      return false;
    }
    if (submitted === (issue.route ?? "")) {
      setRouteDraftDirty(false);
      return false;
    }
    submit({ route: submitted });
    return true;
  }

  function requestParentSubmit(): boolean {
    const submitted = parentRef.current.trim();
    if (issue.closed_at !== null) {
      return false;
    }
    if (submitted === (issue.parent ?? "")) {
      applyServerParent(submitted);
      setParentDraftDirty(false);
      return false;
    }
    // An empty draft clears the parent: the wire form is null, never "".
    submit({ parent: submitted === "" ? null : submitted });
    return true;
  }

  const onIssueSuccess = (next: Issue) => {
    if (titleRef.current.trim() === next.title) {
      applyServerTitle(next.title);
    }
    setTitleDraftDirty(titleRef.current.trim() !== next.title);
    setRouteDraftDirty(routeRef.current !== (next.route ?? ""));
    setParentDraftDirty(parentRef.current.trim() !== (next.parent ?? ""));
  };
  const retry = () => {
    const input = refusal?.input;
    if (input === undefined) {
      return;
    }
    if (input.title !== undefined) {
      requestTitleSubmit();
      return;
    }
    if (input.route !== undefined) {
      requestRouteSubmit();
      return;
    }
    if (input.parent !== undefined) {
      requestParentSubmit();
      return;
    }
    submit(input);
  };
  const discardTitle = () => {
    applyServerTitle(issue.title);
    setTitleDraftDirty(false);
    setTitleError(null);
  };
  const discardRoute = () => {
    applyServerRoute(issue.route ?? "");
    setRouteDraftDirty(false);
  };
  const discardParent = () => {
    applyServerParent(issue.parent ?? "");
    setParentDraftDirty(false);
  };
  const requestStatusSubmit = (status: string): void => submit({ status });

  useEffect(() => {
    if (!titleDirty && !titleDirtyRef.current) {
      applyServerTitle(issue.title);
    }
  }, [applyServerTitle, issue.title, titleDirty]);
  useEffect(() => {
    if (!routeDirty && !routeDirtyRef.current) {
      applyServerRoute(issue.route ?? "");
    }
  }, [applyServerRoute, issue.route, routeDirty]);
  useEffect(() => {
    if (!parentDirty && !parentDirtyRef.current) {
      applyServerParent(issue.parent ?? "");
    }
  }, [applyServerParent, issue.parent, parentDirty]);

  return {
    discardTitle,
    discardRoute,
    discardParent,
    onIssueSuccess,
    refusal,
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
