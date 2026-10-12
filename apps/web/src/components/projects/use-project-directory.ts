"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { listProjects, ProjectRequestError } from "../../lib/project-client.ts";
import type { Project } from "../../lib/projects.ts";

type DirectoryState = {
  projects: Project[];
  nextOffset: number | null;
  loaded: boolean;
  pending: "initial" | "refresh" | "more" | null;
  error: string | null;
  moreError: string | null;
};

function mergeProjects(existing: Project[], incoming: Project[]) {
  const projects = new Map(existing.map((project) => [project.id, project]));
  for (const project of incoming) projects.set(project.id, project);
  return [...projects.values()];
}

export function useProjectDirectory() {
  const requestRef = useRef<AbortController | null>(null);
  const [state, setState] = useState<DirectoryState>({
    projects: [],
    nextOffset: null,
    loaded: false,
    pending: "initial",
    error: null,
    moreError: null,
  });

  const load = useCallback((offset: number, append: boolean) => {
    if (append && requestRef.current) return;
    requestRef.current?.abort();
    const controller = new AbortController();
    requestRef.current = controller;

    return listProjects(offset, controller.signal)
      .then((page) => {
        if (controller.signal.aborted || requestRef.current !== controller)
          return;
        setState((current) => ({
          projects: mergeProjects(
            append ? current.projects : [],
            page.projects,
          ),
          nextOffset: page.next_offset,
          loaded: true,
          pending: null,
          error: null,
          moreError: null,
        }));
      })
      .catch((error: unknown) => {
        if (controller.signal.aborted || requestRef.current !== controller)
          return;
        const message =
          error instanceof ProjectRequestError
            ? error.message
            : "Project service is unavailable. Try again.";
        setState((current) => ({
          ...current,
          pending: null,
          error: append ? current.error : message,
          moreError: append ? message : null,
        }));
      })
      .finally(() => {
        if (requestRef.current === controller) requestRef.current = null;
      });
  }, []);

  const refresh = useCallback(() => {
    setState((current) => ({
      ...current,
      pending: current.loaded ? "refresh" : "initial",
      moreError: null,
    }));
    void load(0, false);
  }, [load]);
  const loadMore = useCallback(() => {
    if (
      state.nextOffset !== null &&
      state.pending === null &&
      !requestRef.current
    ) {
      setState((current) => ({ ...current, pending: "more", moreError: null }));
      void load(state.nextOffset, true);
    }
  }, [load, state.nextOffset, state.pending]);

  useEffect(() => {
    void load(0, false);
    return () => requestRef.current?.abort();
  }, [load]);

  return { ...state, refresh, loadMore };
}
