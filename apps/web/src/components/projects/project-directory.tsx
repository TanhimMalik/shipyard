"use client";

import { ExternalLink, FolderOpen, Plus, RefreshCw } from "lucide-react";
import Link from "next/link";
import { useProjectDirectory } from "./use-project-directory";

const secondaryAction =
  "inline-flex min-h-11 items-center justify-center gap-2 rounded-lg border border-border-strong bg-card px-3 text-sm font-medium hover:bg-muted disabled:cursor-wait disabled:opacity-60";

export function ProjectDirectory() {
  const directory = useProjectDirectory();
  const refreshing =
    directory.pending === "initial" || directory.pending === "refresh";
  const loadingMore = directory.pending === "more";
  const status = directory.pending
    ? loadingMore
      ? "Loading more projects."
      : "Loading projects."
    : directory.loaded
      ? "Projects loaded."
      : "";

  return (
    <section className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">Projects</h1>
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            onClick={directory.refresh}
            disabled={refreshing}
            className={secondaryAction}
          >
            <RefreshCw size={16} aria-hidden="true" />
            {refreshing && directory.loaded ? "Refreshing…" : "Refresh"}
          </button>
          <Link
            href="/projects/new"
            className="inline-flex min-h-11 items-center justify-center gap-2 rounded-lg bg-primary-hover px-4 text-sm font-semibold text-primary-foreground hover:brightness-95"
          >
            <Plus size={16} aria-hidden="true" />
            New project
          </Link>
        </div>
      </div>
      <p role="status" className="sr-only">
        {status}
      </p>

      {!directory.loaded ? (
        directory.error ? (
          <div className="space-y-3 rounded-lg border border-border bg-card p-5">
            <div role={refreshing ? "status" : "alert"}>
              <h2 className="font-semibold">Couldn’t load projects</h2>
              <p className="mt-1 text-sm text-secondary-foreground">
                {refreshing ? "Trying again…" : directory.error}
              </p>
            </div>
            <button
              type="button"
              onClick={directory.refresh}
              disabled={refreshing}
              className={secondaryAction}
            >
              {refreshing ? "Trying again…" : "Retry"}
            </button>
          </div>
        ) : (
          <div
            aria-hidden="true"
            className="divide-y divide-border rounded-lg border border-border bg-card"
          >
            {[0, 1, 2].map((row) => (
              <div
                key={row}
                className="space-y-3 p-4 motion-safe:animate-pulse"
              >
                <div className="h-4 w-36 rounded bg-muted" />
                <div className="h-3 w-2/3 rounded bg-muted" />
              </div>
            ))}
          </div>
        )
      ) : (
        <>
          {directory.error && !refreshing ? (
            <p
              role="alert"
              className="rounded-lg border border-danger bg-card p-3 text-sm text-danger"
            >
              Couldn’t refresh projects. {directory.error}
            </p>
          ) : null}
          {directory.projects.length === 0 ? (
            <div className="rounded-lg border border-dashed border-border-strong bg-card px-5 py-12 text-center">
              <FolderOpen
                size={28}
                aria-hidden="true"
                className="mx-auto mb-3 text-secondary-foreground"
              />
              <h2 className="font-semibold">No projects yet</h2>
              <p className="mx-auto mt-2 max-w-sm text-sm leading-6 text-secondary-foreground">
                Create a project to save its Git repository and application
                settings.
              </p>
            </div>
          ) : (
            <ul
              aria-label="Projects"
              aria-busy={directory.pending !== null}
              className="divide-y divide-border rounded-lg border border-border bg-card"
            >
              {directory.projects.map((project) => (
                <li
                  key={project.id}
                  className="grid min-w-0 gap-4 p-4 sm:grid-cols-2 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.5fr)_minmax(0,1fr)]"
                >
                  <div className="min-w-0">
                    <Link
                      href={`/projects/${project.id}`}
                      className="inline-block max-w-full rounded-sm font-medium [overflow-wrap:anywhere] underline-offset-4 hover:underline"
                    >
                      {project.name}
                    </Link>
                    <p className="mt-1 font-mono text-xs break-all text-secondary-foreground">
                      {project.slug}
                    </p>
                  </div>
                  <div className="min-w-0">
                    <p className="mb-1 text-xs text-secondary-foreground">
                      Repository
                    </p>
                    <a
                      href={project.git_url}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="inline-flex max-w-full items-start gap-1.5 rounded-sm text-sm underline underline-offset-4 hover:text-secondary-foreground"
                    >
                      <span className="min-w-0 break-all">
                        {project.git_url}
                      </span>
                      <ExternalLink
                        size={14}
                        aria-hidden="true"
                        className="mt-1 shrink-0"
                      />
                      <span className="sr-only"> (opens in a new tab)</span>
                    </a>
                  </div>
                  <dl className="flex min-w-0 flex-wrap gap-x-6 gap-y-2 sm:col-span-2 lg:col-span-1">
                    <div className="min-w-0 flex-1">
                      <dt className="mb-1 text-xs text-secondary-foreground">
                        Branch
                      </dt>
                      <dd className="font-mono text-sm break-all">
                        {project.branch}
                      </dd>
                    </div>
                    <div>
                      <dt className="mb-1 text-xs text-secondary-foreground">
                        Port
                      </dt>
                      <dd className="font-mono text-sm">{project.app_port}</dd>
                    </div>
                  </dl>
                </li>
              ))}
            </ul>
          )}
          {directory.nextOffset !== null ? (
            <div className="space-y-3">
              {directory.moreError ? (
                <p role="alert" className="text-sm text-danger">
                  Couldn’t load more projects. {directory.moreError}
                </p>
              ) : null}
              <button
                type="button"
                onClick={directory.loadMore}
                disabled={directory.pending !== null}
                className={secondaryAction}
              >
                {loadingMore ? "Loading more…" : "Load more"}
              </button>
            </div>
          ) : null}
        </>
      )}
    </section>
  );
}
