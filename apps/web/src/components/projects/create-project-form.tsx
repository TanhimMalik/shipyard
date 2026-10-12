"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";
import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  createProject,
  ProjectRequestError,
} from "../../lib/project-client.ts";
import type {
  ProjectField as FieldName,
  ProjectInput,
} from "../../lib/projects.ts";
import { ProjectField } from "./project-field";

type Failure = {
  message: string;
  fields?: Partial<Record<FieldName, string>>;
  checkProjects?: boolean;
};

export function CreateProjectForm() {
  const router = useRouter();
  const formRef = useRef<HTMLFormElement>(null);
  const alertRef = useRef<HTMLDivElement>(null);
  const requestRef = useRef<AbortController | null>(null);
  const [pending, setPending] = useState(false);
  const [failure, setFailure] = useState<Failure | null>(null);

  useEffect(() => () => requestRef.current?.abort(), []);
  useEffect(() => {
    if (!failure || pending) return;
    const field = formRef.current?.querySelector<HTMLInputElement>(
      '[aria-invalid="true"]',
    );
    (field ?? alertRef.current)?.focus();
  }, [failure, pending]);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (pending || requestRef.current) return;
    const data = new FormData(event.currentTarget);
    const port = Number(data.get("app_port"));
    const input: ProjectInput = {
      name: String(data.get("name") ?? ""),
      slug: String(data.get("slug") ?? ""),
      git_url: String(data.get("git_url") ?? ""),
      branch: String(data.get("branch") ?? ""),
      // Let Go return a field error for values that cannot be JSON integers.
      app_port: Number.isSafeInteger(port) ? port : 0,
      health_path: String(data.get("health_path") ?? ""),
    };
    const controller = new AbortController();
    requestRef.current = controller;
    setFailure(null);
    setPending(true);
    let created = false;

    try {
      const project = await createProject(input, controller.signal);
      if (controller.signal.aborted) return;
      router.push(`/projects/${project.id}`);
      created = true;
    } catch (error) {
      if (controller.signal.aborted) return;
      if (error instanceof ProjectRequestError && error.status === 409) {
        setFailure({
          message: "Choose a different project slug.",
          fields: {
            slug: error.fields?.slug ?? "This slug is already in use.",
          },
        });
      } else if (error instanceof ProjectRequestError && error.status === 422) {
        setFailure({
          message: "Check the project settings below.",
          fields: error.fields,
        });
      } else {
        setFailure({
          message:
            error instanceof ProjectRequestError
              ? error.message
              : "We couldn't confirm whether the project was created.",
          checkProjects:
            !(error instanceof ProjectRequestError) ||
            error.status === 0 ||
            error.status >= 500,
        });
      }
    } finally {
      requestRef.current = null;
      if (!controller.signal.aborted && !created) setPending(false);
    }
  }

  return (
    <form
      ref={formRef}
      noValidate
      onSubmit={submit}
      className="space-y-5 rounded-lg border border-border bg-card p-4 sm:p-5"
    >
      <p className="text-sm text-secondary-foreground">
        All fields are required.
      </p>
      {failure ? (
        <div
          ref={alertRef}
          role="alert"
          tabIndex={-1}
          className="space-y-2 rounded-lg border border-danger p-3 text-sm text-danger"
        >
          <p>{failure.message}</p>
          {failure.checkProjects ? (
            <p className="text-secondary-foreground">
              Check{" "}
              <Link
                href="/projects"
                className="font-medium text-foreground underline underline-offset-4"
              >
                Projects
              </Link>{" "}
              before trying again to see whether it was created.
            </p>
          ) : null}
        </div>
      ) : null}
      <fieldset
        disabled={pending}
        aria-busy={pending}
        className="min-w-0 space-y-5"
      >
        <div className="grid gap-5 sm:grid-cols-2">
          <ProjectField
            name="name"
            label="Project name"
            required
            placeholder="My application"
            autoComplete="off"
            error={failure?.fields?.name}
          />
          <ProjectField
            name="slug"
            label="Slug"
            required
            placeholder="my-application"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            hint="Lowercase letters and numbers, separated by single hyphens."
            error={failure?.fields?.slug}
          />
        </div>
        <ProjectField
          name="git_url"
          label="Git repository URL"
          type="url"
          required
          placeholder="https://github.com/owner/repository"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          hint="Use an HTTPS repository URL without credentials, a query, or a fragment."
          error={failure?.fields?.git_url}
        />
        <div className="grid gap-5 sm:grid-cols-2">
          <ProjectField
            name="branch"
            label="Branch"
            required
            placeholder="main"
            autoComplete="off"
            autoCapitalize="none"
            spellCheck={false}
            hint="The branch to use from this repository."
            error={failure?.fields?.branch}
          />
          <ProjectField
            name="app_port"
            label="Internal application port"
            type="number"
            required
            placeholder="8080"
            min={1}
            max={65535}
            step={1}
            inputMode="numeric"
            hint="The port your application listens on inside its container."
            error={failure?.fields?.app_port}
          />
        </div>
        <ProjectField
          name="health_path"
          label="Health check path"
          required
          placeholder="/health"
          autoComplete="off"
          autoCapitalize="none"
          spellCheck={false}
          hint="A path starting with /, without a query or fragment."
          error={failure?.fields?.health_path}
        />
      </fieldset>
      <div className="flex flex-wrap items-center gap-3 border-t border-border pt-5">
        <button
          type="submit"
          disabled={pending}
          className="inline-flex min-h-11 items-center justify-center rounded-lg bg-primary-hover px-4 text-sm font-semibold text-primary-foreground hover:brightness-95 disabled:cursor-wait disabled:opacity-60"
        >
          {pending ? "Creating project…" : "Create project"}
        </button>
        {pending ? (
          <span
            aria-disabled="true"
            className="inline-flex min-h-11 items-center px-3 text-sm text-muted-foreground"
          >
            Cancel
          </span>
        ) : (
          <Link
            href="/projects"
            className="inline-flex min-h-11 items-center rounded-lg px-3 text-sm font-medium hover:bg-muted"
          >
            Cancel
          </Link>
        )}
        <p role="status" className="sr-only">
          {pending ? "Creating project." : ""}
        </p>
      </div>
    </form>
  );
}
