"use client";

import { ArrowLeft, ExternalLink, LoaderCircle } from "lucide-react";
import Link from "next/link";
import { useEffect, useState } from "react";
import { getProject, ProjectRequestError } from "../../lib/project-client.ts";
import type { Project } from "../../lib/projects.ts";

type DetailState =
  | { status: "loading" }
  | { status: "ready"; project: Project }
  | { status: "not-found" }
  | { status: "error" };

export function ProjectDetail({ id }: { id: string }) {
  return <ProjectDetailContent key={id} id={id} />;
}

function ProjectDetailContent({ id }: { id: string }) {
  const [state, setState] = useState<DetailState>({ status: "loading" });
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();

    async function load() {
      try {
        const project = await getProject(id, controller.signal);
        if (!controller.signal.aborted) {
          setState({ status: "ready", project });
        }
      } catch (error) {
        if (!controller.signal.aborted) {
          setState({
            status:
              error instanceof ProjectRequestError &&
              (error.status === 400 || error.status === 404)
                ? "not-found"
                : "error",
          });
        }
      }
    }

    void load();
    return () => controller.abort();
  }, [id, attempt]);

  const project = state.status === "ready" ? state.project : null;

  return (
    <div className="space-y-6">
      <Link
        href="/projects"
        className="inline-flex min-h-11 items-center gap-2 rounded-lg text-sm font-medium text-secondary-foreground hover:text-foreground"
      >
        <ArrowLeft size={16} aria-hidden="true" />
        Back to projects
      </Link>

      <div className="min-w-0">
        <h1 className="text-2xl font-semibold tracking-tight break-words">
          {project
            ? project.name
            : state.status === "not-found"
              ? "Project not found"
              : "Project"}
        </h1>
        {project && (
          <p className="mt-2 font-mono text-sm break-all text-muted-foreground">
            {project.slug}
          </p>
        )}
      </div>

      {state.status === "loading" && (
        <p
          role="status"
          aria-atomic="true"
          className="flex items-center gap-2 text-sm text-muted-foreground"
        >
          <LoaderCircle
            size={16}
            aria-hidden="true"
            className="animate-spin motion-reduce:animate-none"
          />
          Loading project…
        </p>
      )}

      {state.status === "not-found" && (
        <p role="status" className="text-sm text-muted-foreground">
          This project could not be found. Return to projects to choose another.
        </p>
      )}

      {state.status === "error" && (
        <div className="space-y-3 rounded-lg border border-border bg-card p-6">
          <p role="alert" className="text-sm text-secondary-foreground">
            Could not load this project. Try again.
          </p>
          <button
            type="button"
            onClick={() => {
              setState({ status: "loading" });
              setAttempt((current) => current + 1);
            }}
            className="inline-flex min-h-11 items-center justify-center rounded-lg border border-border px-4 text-sm font-medium hover:bg-muted"
          >
            Retry
          </button>
        </div>
      )}

      {project && (
        <dl className="grid min-w-0 gap-x-8 rounded-lg border border-border bg-card px-6 sm:grid-cols-2">
          <div className="min-w-0 border-b border-border py-4 sm:col-span-2">
            <dt className="text-sm text-muted-foreground">Git repository</dt>
            <dd className="mt-2 min-w-0 text-sm">
              <a
                href={project.git_url}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex max-w-full items-start gap-2 text-primary underline-offset-4 hover:underline"
              >
                <span className="min-w-0 font-mono break-all">
                  {project.git_url}
                </span>
                <ExternalLink
                  size={14}
                  aria-hidden="true"
                  className="mt-0.5 shrink-0"
                />
                <span className="sr-only">(opens in a new tab)</span>
              </a>
            </dd>
          </div>
          <div className="min-w-0 border-b border-border py-4">
            <dt className="text-sm text-muted-foreground">Branch</dt>
            <dd className="mt-2 font-mono text-sm break-all">
              {project.branch}
            </dd>
          </div>
          <div className="min-w-0 border-b border-border py-4">
            <dt className="text-sm text-muted-foreground">Application port</dt>
            <dd className="mt-2 font-mono text-sm">{project.app_port}</dd>
          </div>
          <div className="min-w-0 border-b border-border py-4 sm:col-span-2">
            <dt className="text-sm text-muted-foreground">Health path</dt>
            <dd className="mt-2 font-mono text-sm break-all">
              {project.health_path}
            </dd>
          </div>
          <div className="min-w-0 border-b border-border py-4">
            <dt className="text-sm text-muted-foreground">Created</dt>
            <dd className="mt-2 text-sm">
              <time dateTime={project.created_at}>
                {formatTimestamp(project.created_at)}
              </time>
            </dd>
          </div>
          <div className="min-w-0 border-b border-border py-4">
            <dt className="text-sm text-muted-foreground">Updated</dt>
            <dd className="mt-2 text-sm">
              <time dateTime={project.updated_at}>
                {formatTimestamp(project.updated_at)}
              </time>
            </dd>
          </div>
          <div className="min-w-0 py-4 sm:col-span-2">
            <dt className="text-sm text-muted-foreground">Project ID</dt>
            <dd className="mt-2 font-mono text-sm break-all">{project.id}</dd>
          </div>
        </dl>
      )}
    </div>
  );
}

function formatTimestamp(value: string) {
  return new Date(value).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  });
}
