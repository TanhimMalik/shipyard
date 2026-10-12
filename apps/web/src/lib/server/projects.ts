import { isIP } from "node:net";
import {
  isProject,
  isProjectPage,
  projectFields,
  projectIDPattern,
  type Project,
  type ProjectField,
} from "../projects.ts";

const unavailable = "Project service is unavailable. Try again.";

function reply(body: unknown, status: number) {
  return Response.json(body, {
    status,
    headers: {
      "Cache-Control": "no-store",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

function localHostname(hostname: string) {
  const host = hostname.replace(/^\[|\]$/g, "");
  return (
    host === "localhost" ||
    host === "::1" ||
    (isIP(host) === 4 && host.split(".")[0] === "127")
  );
}

function plainLocalOrigin(value: string) {
  const url = new URL(value);
  if (
    !["http:", "https:"].includes(url.protocol) ||
    !localHostname(url.hostname) ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    url.pathname !== "/" ||
    url.port === "0"
  ) {
    throw new Error("Invalid local origin");
  }
  return url;
}

class BodyLimitError extends Error {}

async function readText(
  body: ReadableStream<Uint8Array> | null,
  limit: number,
  signal: AbortSignal,
) {
  signal.throwIfAborted();
  if (!body) return "";
  const reader = body.getReader();
  const chunks: Uint8Array[] = [];
  let length = 0;
  const cancel = () => void reader.cancel().catch(() => {});
  signal.addEventListener("abort", cancel, { once: true });
  try {
    while (true) {
      const chunk = await reader.read();
      signal.throwIfAborted();
      if (chunk.done) break;
      length += chunk.value.byteLength;
      if (length > limit) {
        await reader.cancel();
        throw new BodyLimitError();
      }
      chunks.push(chunk.value);
    }
    const bytes = new Uint8Array(length);
    let offset = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, offset);
      offset += chunk.byteLength;
    }
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } finally {
    signal.removeEventListener("abort", cancel);
    reader.releaseLock();
  }
}

function validationFields(body: unknown) {
  if (typeof body !== "object" || body === null || !("fields" in body)) {
    return undefined;
  }
  if (typeof body.fields !== "object" || body.fields === null) return undefined;
  const input = body.fields as Record<string, unknown>;
  const fields: Partial<Record<ProjectField, string>> = {};
  for (const field of projectFields) {
    const message = input[field];
    if (
      typeof message === "string" &&
      message.length > 0 &&
      message.length <= 200
    ) {
      fields[field] = message;
    }
  }
  return fields;
}

function publicProject(project: Project): Project {
  return {
    id: project.id,
    name: project.name,
    slug: project.slug,
    git_url: project.git_url,
    branch: project.branch,
    app_port: project.app_port,
    health_path: project.health_path,
    created_at: project.created_at,
    updated_at: project.updated_at,
  };
}

export async function proxyProjects(request: Request, id?: string) {
  // This guard applies to every project read and write. The health route has
  // a separate policy and may reach a remote API before admin access exists.
  if (process.env.VERCEL !== undefined) {
    return reply(
      { error: "Project management is unavailable on this deployment." },
      503,
    );
  }
  let requestURL: URL;
  let incomingOrigin: URL;
  try {
    requestURL = new URL(request.url);
    if (
      !localHostname(requestURL.hostname) ||
      !["http:", "https:"].includes(requestURL.protocol)
    ) {
      throw new Error("Nonlocal request");
    }
    const host = request.headers.get("Host");
    if (!host || /[\s/@?#\\]/.test(host)) throw new Error("Invalid Host");
    incomingOrigin = plainLocalOrigin(`${requestURL.protocol}//${host}`);
  } catch {
    return reply(
      { error: "Project access is unavailable from this host." },
      403,
    );
  }
  const method = request.method === "HEAD" ? "GET" : request.method;
  if (
    (method !== "GET" && method !== "POST") ||
    (id !== undefined && method !== "GET")
  ) {
    return reply({ error: "Method not allowed." }, 405);
  }
  if (id !== undefined && !projectIDPattern.test(id)) {
    return reply({ error: "Use a UUID project ID." }, 400);
  }
  if (method === "POST") {
    const site = request.headers.get("Sec-Fetch-Site");
    const origin = request.headers.get("Origin");
    if (
      (site !== null && site !== "same-origin" && site !== "none") ||
      (origin !== null && origin !== incomingOrigin.origin)
    ) {
      return reply(
        { error: "Cross-origin project requests are not allowed." },
        403,
      );
    }
  }
  let apiOrigin: URL;
  try {
    apiOrigin = plainLocalOrigin(
      process.env.SHIPYARD_API_URL ??
        (process.env.NODE_ENV === "production" ? "" : "http://127.0.0.1:8080"),
    );
    if (
      apiOrigin.origin === incomingOrigin.origin ||
      apiOrigin.origin === requestURL.origin
    ) {
      throw new Error("Recursive API origin");
    }
  } catch {
    return reply({ error: unavailable }, 503);
  }
  const signal = AbortSignal.any([request.signal, AbortSignal.timeout(4000)]);
  let body: string | undefined;
  if (method === "POST") {
    if (
      request.headers
        .get("Content-Type")
        ?.split(";")[0]
        ?.trim()
        .toLowerCase() !== "application/json"
    ) {
      return reply({ error: "Use application/json." }, 415);
    }
    if (Number(request.headers.get("Content-Length")) > 64 * 1024) {
      return reply({ error: "Project settings are too large." }, 413);
    }
    try {
      body = await readText(request.body, 64 * 1024, signal);
    } catch (error) {
      return reply(
        {
          error:
            error instanceof BodyLimitError
              ? "Project settings are too large."
              : "Could not read project settings.",
        },
        error instanceof BodyLimitError ? 413 : 400,
      );
    }
  }
  const upstreamURL = new URL(
    id === undefined ? "/api/projects" : `/api/projects/${id}`,
    apiOrigin,
  );
  if (id === undefined && method === "GET")
    upstreamURL.search = requestURL.search;
  try {
    const response = await fetch(upstreamURL, {
      method,
      headers:
        method === "POST"
          ? { Accept: "application/json", "Content-Type": "application/json" }
          : { Accept: "application/json" },
      body,
      cache: "no-store",
      redirect: "error",
      signal,
    });
    const expectedStatus = method === "POST" ? 201 : 200;
    if (response.status === expectedStatus) {
      const data: unknown = JSON.parse(
        await readText(response.body, 2 * 1024 * 1024, signal),
      );
      if (id === undefined && method === "GET") {
        const offset = Number(requestURL.searchParams.get("offset") ?? 0);
        if (
          !isProjectPage(data) ||
          (data.next_offset !== null && data.next_offset <= offset)
        )
          throw new Error("Invalid project page");
        return reply(
          {
            projects: data.projects.map(publicProject),
            next_offset: data.next_offset,
          },
          expectedStatus,
        );
      }
      if (!isProject(data)) throw new Error("Invalid project");
      return reply(publicProject(data), expectedStatus);
    }
    const messages: Record<number, string> = {
      400: "Check the project request.",
      404: "Project not found.",
      409: "A project with this slug already exists.",
      413: "Project settings are too large.",
      415: "Use application/json.",
      422: "Check the project settings.",
    };
    const message = messages[response.status];
    if (message) {
      let fields: Partial<Record<ProjectField, string>> | undefined;
      if (response.status === 422) {
        fields = validationFields(
          JSON.parse(await readText(response.body, 16 * 1024, signal)),
        );
      } else {
        await response.body?.cancel();
      }
      return reply(
        { error: message, ...(fields ? { fields } : {}) },
        response.status,
      );
    }
    await response.body?.cancel();
  } catch {
    // Upstream errors can include private configuration; return only safe text.
  }
  return reply({ error: unavailable }, 503);
}
