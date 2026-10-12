import {
  isProject,
  isProjectPage,
  projectFields,
  type Project,
  type ProjectField,
  type ProjectInput,
  type ProjectPage,
} from "./projects.ts";

export class ProjectRequestError extends Error {
  status: number;
  fields?: Partial<Record<ProjectField, string>>;

  constructor(
    message: string,
    status: number,
    fields?: Partial<Record<ProjectField, string>>,
  ) {
    super(message);
    this.name = "ProjectRequestError";
    this.status = status;
    this.fields = fields;
  }
}

async function requestProjectData(
  path: string,
  signal?: AbortSignal,
  input?: ProjectInput,
) {
  const controller = new AbortController();
  const abort = () => controller.abort();
  if (signal?.aborted) abort();
  else signal?.addEventListener("abort", abort, { once: true });
  const timeout = setTimeout(abort, 5000);
  try {
    const response = await fetch(path, {
      method: input ? "POST" : "GET",
      cache: "no-store",
      headers: input ? { "Content-Type": "application/json" } : undefined,
      body: input ? JSON.stringify(input) : undefined,
      signal: controller.signal,
    });
    const body: unknown = await response.json();
    if (!response.ok) {
      const error = body as {
        error?: unknown;
        fields?: Record<string, unknown>;
      } | null;
      const fields: Partial<Record<ProjectField, string>> = {};
      for (const field of projectFields) {
        const message = error?.fields?.[field];
        if (typeof message === "string" && message.length <= 200)
          fields[field] = message;
      }
      throw new ProjectRequestError(
        typeof error?.error === "string" && error.error.length <= 200
          ? error.error
          : "Project service is unavailable. Try again.",
        response.status,
        fields,
      );
    }
    return body;
  } catch (error) {
    if (error instanceof ProjectRequestError) throw error;
    throw new ProjectRequestError(
      "Project service is unavailable. Try again.",
      503,
    );
  } finally {
    clearTimeout(timeout);
    signal?.removeEventListener("abort", abort);
  }
}

export async function listProjects(
  offset = 0,
  signal?: AbortSignal,
): Promise<ProjectPage> {
  const page = await requestProjectData(
    `/api/projects?limit=20&offset=${offset}`,
    signal,
  );
  if (!isProjectPage(page))
    throw new ProjectRequestError("Could not read projects. Try again.", 503);
  return page;
}

export async function getProject(
  id: string,
  signal?: AbortSignal,
): Promise<Project> {
  const project = await requestProjectData(
    `/api/projects/${encodeURIComponent(id)}`,
    signal,
  );
  if (!isProject(project))
    throw new ProjectRequestError(
      "Could not read this project. Try again.",
      503,
    );
  return project;
}

export async function createProject(
  input: ProjectInput,
  signal?: AbortSignal,
): Promise<Project> {
  const project = await requestProjectData("/api/projects", signal, input);
  if (!isProject(project))
    throw new ProjectRequestError("Could not confirm project creation.", 503);
  return project;
}
