import assert from "node:assert/strict";
import { describe, test } from "node:test";
import {
  createProject,
  getProject,
  listProjects,
  ProjectRequestError,
} from "./project-client.ts";
import type { Project, ProjectInput } from "./projects.ts";

function createFixture(): Project {
  return {
    id: "b03d6a37-22cb-4c53-9328-2b4327caa221",
    name: "Example project",
    slug: "example-project",
    git_url: "https://example.com/team/project.git",
    branch: "main",
    app_port: 8080,
    health_path: "/ready",
    created_at: "2026-10-10T10:00:00Z",
    updated_at: "2026-10-10T10:00:00Z",
  };
}

function createInput(): ProjectInput {
  const project = createFixture();
  return {
    name: project.name,
    slug: project.slug,
    git_url: project.git_url,
    branch: project.branch,
    app_port: project.app_port,
    health_path: project.health_path,
  };
}

function assertRequestError(
  error: unknown,
  status: number,
): asserts error is ProjectRequestError {
  assert.ok(error instanceof ProjectRequestError);
  assert.equal(error.name, "ProjectRequestError");
  assert.equal(error.status, status);
}

describe("project browser client", { concurrency: false }, () => {
  test("lists through the relative gateway with fixed page size and caller offset", async (t) => {
    const page = { projects: [createFixture()], next_offset: 20 };
    const paths: string[] = [];
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (input: RequestInfo | URL, options?: RequestInit) => {
        paths.push(String(input));
        assert.equal(options?.method, "GET");
        assert.equal(options?.cache, "no-store");
        assert.equal(options?.body, undefined);
        assert.ok(options?.signal instanceof AbortSignal);
        assert.equal(options.signal.aborted, false);
        return Response.json(page);
      },
    );

    assert.deepEqual(await listProjects(), page);
    assert.deepEqual(await listProjects(20), page);
    assert.deepEqual(paths, [
      "/api/projects?limit=20&offset=0",
      "/api/projects?limit=20&offset=20",
    ]);
    assert.equal(fetchMock.mock.callCount(), 2);
  });

  test("encodes the entire detail identifier as one path segment", async (t) => {
    const project = createFixture();
    const identifier = `${project.id}/?unexpected=query`;
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (input: RequestInfo | URL, options?: RequestInit) => {
        assert.equal(
          String(input),
          `/api/projects/${encodeURIComponent(identifier)}`,
        );
        assert.equal(options?.method, "GET");
        assert.equal(options?.cache, "no-store");
        return Response.json(project);
      },
    );

    assert.deepEqual(await getProject(identifier), project);
    assert.equal(fetchMock.mock.callCount(), 1);
  });

  test("creates with one JSON POST and removes the caller's abort listener after completion", async (t) => {
    const project = createFixture();
    const input = createInput();
    const controller = new AbortController();
    let upstreamSignal: AbortSignal | undefined;
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (path: RequestInfo | URL, options?: RequestInit) => {
        assert.ok(options);
        assert.equal(String(path), "/api/projects");
        assert.equal(options?.method, "POST");
        assert.equal(options?.cache, "no-store");
        assert.equal(
          new Headers(options?.headers).get("Content-Type"),
          "application/json",
        );
        assert.equal(typeof options?.body, "string");
        assert.deepEqual(JSON.parse(String(options.body)), input);
        assert.ok(options.signal instanceof AbortSignal);
        upstreamSignal = options.signal;
        return Response.json(project, { status: 201 });
      },
    );

    assert.deepEqual(await createProject(input, controller.signal), project);
    assert.equal(fetchMock.mock.callCount(), 1);
    assert.ok(upstreamSignal);
    controller.abort();
    assert.equal(upstreamSignal.aborted, false);
  });

  test("preserves the response status and only permitted bounded field messages", async (t) => {
    t.mock.method(globalThis, "fetch", async () =>
      Response.json(
        {
          error: "Check the project settings.",
          fields: {
            name: "Required.",
            app_port: "Use a port between 1 and 65535.",
            slug: "x".repeat(201),
            branch: 7,
            unexpected: "private detail",
          },
        },
        { status: 422 },
      ),
    );

    await assert.rejects(createProject(createInput()), (error: unknown) => {
      assertRequestError(error, 422);
      assert.equal(error.message, "Check the project settings.");
      assert.deepEqual(error.fields, {
        name: "Required.",
        app_port: "Use a port between 1 and 65535.",
      });
      return true;
    });
  });

  test("uses a safe fallback when an API error message is malformed", async (t) => {
    t.mock.method(globalThis, "fetch", async () =>
      Response.json({ error: { detail: "private detail" } }, { status: 409 }),
    );

    await assert.rejects(createProject(createInput()), (error: unknown) => {
      assertRequestError(error, 409);
      assert.equal(error.message, "Project service is unavailable. Try again.");
      assert.doesNotMatch(error.message, /private detail/);
      return true;
    });
  });

  test("rejects malformed JSON and invalid success payloads without retrying an unconfirmed create", async (t) => {
    const cases = [
      {
        name: "malformed JSON",
        call: () => listProjects(),
        response: () => new Response("{"),
        message: "Project service is unavailable. Try again.",
      },
      {
        name: "invalid page",
        call: () => listProjects(),
        response: () => Response.json({ projects: [], next_offset: "20" }),
        message: "Could not read projects. Try again.",
      },
      {
        name: "invalid detail",
        call: () => getProject(createFixture().id),
        response: () => Response.json({ ...createFixture(), app_port: "8080" }),
        message: "Could not read this project. Try again.",
      },
      {
        name: "unconfirmed create",
        call: () => createProject(createInput()),
        response: () =>
          Response.json({ id: createFixture().id }, { status: 201 }),
        message: "Could not confirm project creation.",
      },
    ];
    for (const entry of cases) {
      await t.test(entry.name, async (t) => {
        const fetchMock = t.mock.method(globalThis, "fetch", async () =>
          entry.response(),
        );
        await assert.rejects(entry.call(), (error: unknown) => {
          assertRequestError(error, 503);
          assert.equal(error.message, entry.message);
          return true;
        });
        assert.equal(fetchMock.mock.callCount(), 1);
      });
    }
  });

  test(
    "propagates caller cancellation to the pending fetch and returns a safe error",
    { timeout: 2_000 },
    async (t) => {
      const controller = new AbortController();
      let notifyFetch: (() => void) | undefined;
      const started = new Promise<void>((resolve) => {
        notifyFetch = resolve;
      });
      let upstreamSignal: AbortSignal | undefined;
      const fetchMock = t.mock.method(
        globalThis,
        "fetch",
        async (_input: RequestInfo | URL, options?: RequestInit) => {
          const signal = options?.signal;
          assert.ok(signal instanceof AbortSignal);
          upstreamSignal = signal;
          notifyFetch?.();
          return new Promise<Response>((_resolve, reject) => {
            if (signal.aborted) reject(signal.reason);
            else
              signal.addEventListener("abort", () => reject(signal.reason), {
                once: true,
              });
          });
        },
      );

      const pending = getProject(createFixture().id, controller.signal);
      await started;
      controller.abort(new Error("private cancellation detail"));
      await assert.rejects(pending, (error: unknown) => {
        assertRequestError(error, 503);
        assert.equal(
          error.message,
          "Project service is unavailable. Try again.",
        );
        assert.doesNotMatch(error.message, /private cancellation detail/);
        return true;
      });
      assert.equal(upstreamSignal?.aborted, true);
      assert.equal(fetchMock.mock.callCount(), 1);
    },
  );

  test("does not automatically retry a POST after a network failure", async (t) => {
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (_input: RequestInfo | URL, options?: RequestInit) => {
        assert.equal(options?.method, "POST");
        throw new TypeError("private network address", {
          cause: new Error("internal detail"),
        });
      },
    );

    await assert.rejects(createProject(createInput()), (error: unknown) => {
      assertRequestError(error, 503);
      assert.equal(error.message, "Project service is unavailable. Try again.");
      assert.doesNotMatch(error.message, /private|internal/);
      return true;
    });
    assert.equal(fetchMock.mock.callCount(), 1);
  });
});
