import assert from "node:assert/strict";
import { createServer } from "node:http";
import { describe, test, type TestContext } from "node:test";
import { proxyProjects } from "./projects.ts";

const collectionUrl = "http://127.0.0.1:3000/api/projects";
const apiOrigin = "http://127.0.0.1:8080";
const project = {
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
const createInput = {
  name: project.name,
  slug: project.slug,
  git_url: project.git_url,
  branch: project.branch,
  app_port: project.app_port,
  health_path: project.health_path,
};
const emptyList = { projects: [], next_offset: null };

function configureEnvironment(
  t: TestContext,
  apiUrl: string | undefined,
  nodeEnv = "production",
  vercel?: string,
) {
  const previous = {
    NODE_ENV: process.env.NODE_ENV,
    SHIPYARD_API_URL: process.env.SHIPYARD_API_URL,
    VERCEL: process.env.VERCEL,
  };
  t.after(() => {
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) Reflect.deleteProperty(process.env, key);
      else Reflect.set(process.env, key, value);
    }
  });
  Reflect.set(process.env, "NODE_ENV", nodeEnv);
  for (const [key, value] of Object.entries({
    SHIPYARD_API_URL: apiUrl,
    VERCEL: vercel,
  })) {
    if (value === undefined) Reflect.deleteProperty(process.env, key);
    else Reflect.set(process.env, key, value);
  }
}

function localRequest(init: RequestInit = {}, url = collectionUrl) {
  const headers = new Headers(init.headers);
  if (!headers.has("Host")) headers.set("Host", "127.0.0.1:3000");
  return new Request(url, { ...init, headers });
}

function postRequest(headers: HeadersInit = {}) {
  return localRequest({
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...Object.fromEntries(new Headers(headers)),
    },
    body: JSON.stringify(createInput),
  });
}

function assertResponseHeaders(response: Response) {
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.equal(response.headers.get("X-Content-Type-Options"), "nosniff");
  assert.match(
    response.headers.get("Content-Type") ?? "",
    /^application\/json\b/,
  );
  assert.equal(response.headers.get("Set-Cookie"), null);
  assert.equal(response.headers.get("X-Upstream-Debug"), null);
}

async function assertError(response: Response, status: number) {
  assert.equal(response.status, status);
  assertResponseHeaders(response);
  const payload: unknown = await response.json();
  assert.ok(payload && typeof payload === "object" && !Array.isArray(payload));
  const record = payload as Record<string, unknown>;
  assert.equal(typeof record.error, "string");
  if (status !== 422) assert.equal(record.fields, undefined);
  assert.ok(String(record.error).length > 0);
  assert.deepEqual(
    Object.keys(record).filter((key) => key !== "error" && key !== "fields"),
    [],
  );
  assert.doesNotMatch(
    JSON.stringify(record),
    /sql-secret|private-api|session-secret|raw-detail/i,
  );
  return record;
}

function rejectUnexpectedFetch(t: TestContext) {
  return t.mock.method(globalThis, "fetch", async () => {
    throw new Error("unexpected upstream request");
  });
}

describe("local project gateway", { concurrency: false }, () => {
  test("forwards pagination with fresh requests and strips browser credentials", async (t) => {
    configureEnvironment(t, apiOrigin);
    const payload = { projects: [project], next_offset: 5 };
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (input: RequestInfo | URL, options?: RequestInit) => {
        assert.equal(
          String(input),
          `${apiOrigin}/api/projects?limit=1&offset=4`,
        );
        assert.equal(options?.cache, "no-store");
        assert.equal(options?.redirect, "error");
        assert.ok(options?.signal instanceof AbortSignal);
        assert.equal(options.signal.aborted, false);
        const outbound = new Request(input, options);
        assert.equal(outbound.method, "GET");
        for (const header of ["Authorization", "Cookie", "Origin"]) {
          assert.equal(outbound.headers.get(header), null);
        }
        return Response.json(
          {
            ...payload,
            projects: [{ ...project, internal_token: "private-api" }],
            debug: "sql-secret",
          },
          {
            headers: {
              "Set-Cookie": "session-secret",
              "X-Upstream-Debug": "sql-secret",
            },
          },
        );
      },
    );
    const response = await proxyProjects(
      localRequest(
        {
          headers: {
            Authorization: "Bearer secret",
            Cookie: "session-secret",
            Origin: "http://127.0.0.1:3000",
          },
        },
        `${collectionUrl}?limit=1&offset=4`,
      ),
    );
    assert.equal(fetchMock.mock.callCount(), 1);
    assert.equal(response.status, 200);
    assertResponseHeaders(response);
    assert.deepEqual(await response.json(), payload);
  });

  test("reads a detail project and treats HEAD as an upstream GET", async (t) => {
    for (const method of ["GET", "HEAD"]) {
      await t.test(method, async (t) => {
        configureEnvironment(t, apiOrigin);
        const fetchMock = t.mock.method(
          globalThis,
          "fetch",
          async (input: RequestInfo | URL, options?: RequestInit) => {
            assert.equal(
              String(input),
              `${apiOrigin}/api/projects/${project.id}`,
            );
            assert.equal(new Request(input, options).method, "GET");
            return Response.json(project);
          },
        );
        const response = await proxyProjects(
          localRequest({ method }, `${collectionUrl}/${project.id}`),
          project.id,
        );
        assert.equal(response.status, 200);
        assertResponseHeaders(response);
        if (method === "GET") assert.deepEqual(await response.json(), project);
        assert.equal(fetchMock.mock.callCount(), 1);
      });
    }
  });

  test("allows local CLI creation without Origin and forwards only JSON settings", async (t) => {
    configureEnvironment(t, apiOrigin);
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (input: RequestInfo | URL, options?: RequestInit) => {
        assert.equal(String(input), `${apiOrigin}/api/projects`);
        assert.equal(options?.cache, "no-store");
        assert.equal(options?.redirect, "error");
        const outbound = new Request(input, options);
        assert.equal(outbound.method, "POST");
        assert.match(
          outbound.headers.get("Content-Type") ?? "",
          /^application\/json\b/,
        );
        for (const header of ["Authorization", "Cookie", "Origin"])
          assert.equal(outbound.headers.get(header), null);
        assert.deepEqual(await outbound.json(), createInput);
        return Response.json(project, { status: 201 });
      },
    );
    const response = await proxyProjects(
      postRequest({ Authorization: "Bearer secret", Cookie: "session-secret" }),
    );
    assert.equal(response.status, 201);
    assertResponseHeaders(response);
    assert.deepEqual(await response.json(), project);
    assert.equal(fetchMock.mock.callCount(), 1);
  });

  test("matches browser Origin against the local Host rather than the internal URL hostname", async (t) => {
    configureEnvironment(t, apiOrigin);
    const fetchMock = t.mock.method(globalThis, "fetch", async () =>
      Response.json(project, { status: 201 }),
    );
    const response = await proxyProjects(
      postRequest({
        Host: "localhost:3000",
        Origin: "http://localhost:3000",
        "Sec-Fetch-Site": "same-origin",
      }),
    );
    assert.equal(response.status, 201);
    assert.equal(fetchMock.mock.callCount(), 1);
  });

  test("uses the development API default and accepts native IPv6 loopback", async (t) => {
    await t.test("development default", async (t) => {
      configureEnvironment(t, undefined, "development");
      const fetchMock = t.mock.method(
        globalThis,
        "fetch",
        async (input: RequestInfo | URL) => {
          assert.equal(String(input), `${apiOrigin}/api/projects`);
          return Response.json(emptyList);
        },
      );
      assert.equal((await proxyProjects(localRequest())).status, 200);
      assert.equal(fetchMock.mock.callCount(), 1);
    });
    await t.test("IPv6", async (t) => {
      configureEnvironment(t, "http://[::1]:8080");
      t.mock.method(globalThis, "fetch", async () => Response.json(emptyList));
      assert.equal(
        (
          await proxyProjects(
            localRequest(
              { headers: { Host: "[::1]:3000" } },
              "http://[::1]:3000/api/projects",
            ),
          )
        ).status,
        200,
      );
    });
  });

  test("blocks every defined VERCEL value even with a spoofed localhost Host", async (t) => {
    for (const value of ["", "0", "1"]) {
      await t.test(JSON.stringify(value), async (t) => {
        configureEnvironment(t, apiOrigin, "production", value);
        const fetchMock = rejectUnexpectedFetch(t);
        await assertError(
          await proxyProjects(
            localRequest({ headers: { Host: "localhost:3000" } }),
          ),
          503,
        );
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("protects collection reads and detail reads against remote URL or Host values", async (t) => {
    const cases = [
      {
        request: localRequest({}, "http://public.example/api/projects"),
        id: undefined,
      },
      {
        request: localRequest(
          { headers: { Host: "public.example" } },
          `${collectionUrl}/${project.id}`,
        ),
        id: project.id,
      },
      { request: new Request(collectionUrl), id: undefined },
    ];
    for (const [index, entry] of cases.entries()) {
      await t.test(`access guard ${index + 1}`, async (t) => {
        configureEnvironment(t, apiOrigin);
        const fetchMock = rejectUnexpectedFetch(t);
        await assertError(await proxyProjects(entry.request, entry.id), 403);
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("rejects missing production configuration, remote APIs, non-origins, and recursive targets", async (t) => {
    for (const origin of [
      undefined,
      "",
      "https://private-api.example",
      "http://user:password@127.0.0.1:8080",
      `${apiOrigin}/prefix`,
      "http://127.0.0.1:3000/",
    ]) {
      await t.test(origin ?? "missing", async (t) => {
        configureEnvironment(t, origin);
        const fetchMock = rejectUnexpectedFetch(t);
        await assertError(await proxyProjects(localRequest()), 503);
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("rejects invalid detail IDs before fetching", async (t) => {
    configureEnvironment(t, apiOrigin);
    const fetchMock = rejectUnexpectedFetch(t);
    await assertError(
      await proxyProjects(
        localRequest({}, `${collectionUrl}/not-a-uuid`),
        "not-a-uuid",
      ),
      400,
    );
    assert.equal(fetchMock.mock.callCount(), 0);
  });

  test("rejects cross-site and same-site writes and mismatched local browser origins", async (t) => {
    const headers: HeadersInit[] = [
      { "Sec-Fetch-Site": "cross-site", Origin: "http://127.0.0.1:3000" },
      { "Sec-Fetch-Site": "same-site", Origin: "http://127.0.0.1:3000" },
      { Origin: "http://localhost:3000" },
      { Origin: "https://127.0.0.1:3000" },
    ];
    for (const [index, header] of headers.entries()) {
      await t.test(`CSRF guard ${index + 1}`, async (t) => {
        configureEnvironment(t, apiOrigin);
        const fetchMock = rejectUnexpectedFetch(t);
        await assertError(await proxyProjects(postRequest(header)), 403);
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("requires JSON and rejects an oversized declared body before fetching", async (t) => {
    for (const [name, request, status] of [
      [
        "media type",
        localRequest({
          method: "POST",
          headers: { "Content-Type": "text/plain" },
          body: "{}",
        }),
        415,
      ],
      ["declared size", postRequest({ "Content-Length": "65537" }), 413],
    ] as const) {
      await t.test(name, async (t) => {
        configureEnvironment(t, apiOrigin);
        const fetchMock = rejectUnexpectedFetch(t);
        await assertError(await proxyProjects(request), status);
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("bounds streamed bodies without Content-Length and cancels the remaining input", async (t) => {
    configureEnvironment(t, apiOrigin);
    let chunksRead = 0;
    let canceled = false;
    const body = new ReadableStream<Uint8Array>({
      pull(controller) {
        if (chunksRead === 64) return controller.close();
        chunksRead += 1;
        controller.enqueue(new Uint8Array(16 * 1024).fill(32));
      },
      cancel() {
        canceled = true;
      },
    });
    const init: RequestInit & { duplex: "half" } = {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body,
      duplex: "half",
    };
    const request = localRequest(init);
    assert.equal(request.headers.get("Content-Length"), null);
    const fetchMock = rejectUnexpectedFetch(t);
    await assertError(await proxyProjects(request), 413);
    assert.equal(fetchMock.mock.callCount(), 0);
    assert.equal(canceled, true);
    assert.ok(
      chunksRead <= 6,
      `read ${chunksRead} chunks instead of stopping at the 64 KiB bound`,
    );
  });

  test("maps expected API errors to safe messages without upstream headers or details", async (t) => {
    for (const status of [400, 404, 409, 413, 415]) {
      await t.test(String(status), async (t) => {
        configureEnvironment(t, apiOrigin);
        t.mock.method(globalThis, "fetch", async () =>
          Response.json(
            {
              error: "sql-secret",
              detail: "raw-detail",
              fields: { name: "raw-detail" },
            },
            {
              status,
              headers: {
                "Set-Cookie": "session-secret",
                "X-Upstream-Debug": "sql-secret",
              },
            },
          ),
        );
        const payload = await assertError(
          await proxyProjects(postRequest()),
          status,
        );
        assert.equal(payload.fields, undefined);
      });
    }
  });

  test("preserves only permitted bounded validation messages", async (t) => {
    configureEnvironment(t, apiOrigin);
    t.mock.method(globalThis, "fetch", async () =>
      Response.json(
        {
          error: "sql-secret",
          detail: "raw-detail",
          fields: {
            name: "Required.",
            app_port: "Use a port between 1 and 65535.",
            unknown: "raw-detail",
            branch: 7,
            slug: "x".repeat(201),
          },
        },
        { status: 422 },
      ),
    );
    const payload = await assertError(await proxyProjects(postRequest()), 422);
    assert.ok(
      payload.fields &&
        typeof payload.fields === "object" &&
        !Array.isArray(payload.fields),
    );
    const fields = payload.fields as Record<string, unknown>;
    assert.equal(fields.name, "Required.");
    assert.equal(fields.app_port, "Use a port between 1 and 65535.");
    for (const [key, message] of Object.entries(fields)) {
      assert.ok(
        [
          "name",
          "slug",
          "git_url",
          "branch",
          "app_port",
          "health_path",
        ].includes(key),
      );
      assert.equal(typeof message, "string");
      assert.ok(String(message).length <= 200);
    }
    assert.equal(fields.unknown, undefined);
    assert.equal(fields.branch, undefined);
  });

  test("fails closed on malformed JSON, invalid success shapes, and unexpected statuses", async (t) => {
    const cases = [
      {
        name: "malformed JSON",
        response: new Response("{", { status: 200 }),
        detail: false,
        post: false,
      },
      {
        name: "wrong collection",
        response: Response.json({ projects: "sql-secret", next_offset: null }),
        detail: false,
        post: false,
      },
      {
        name: "invalid pagination",
        response: Response.json({ projects: [], next_offset: -1 }),
        detail: false,
        post: false,
      },
      {
        name: "invalid project field",
        response: Response.json({ ...project, app_port: "8080" }),
        detail: true,
        post: false,
      },
      {
        name: "wrong POST status",
        response: Response.json(project),
        detail: false,
        post: true,
      },
      {
        name: "unexpected status",
        response: Response.json(
          { error: "sql-secret", detail: "raw-detail" },
          { status: 500 },
        ),
        detail: false,
        post: false,
      },
    ];
    for (const entry of cases) {
      await t.test(entry.name, async (t) => {
        configureEnvironment(t, apiOrigin);
        t.mock.method(globalThis, "fetch", async () => entry.response);
        const request = entry.post
          ? postRequest()
          : localRequest(
              {},
              entry.detail ? `${collectionUrl}/${project.id}` : collectionUrl,
            );
        await assertError(
          await proxyProjects(request, entry.detail ? project.id : undefined),
          503,
        );
      });
    }
  });

  test("hides network failure details", async (t) => {
    configureEnvironment(t, apiOrigin);
    t.mock.method(globalThis, "fetch", async () => {
      throw new TypeError("fetch failed at private-api", {
        cause: new Error("sql-secret"),
      });
    });
    await assertError(await proxyProjects(localRequest()), 503);
  });

  test(
    "includes request cancellation in the upstream signal",
    { timeout: 2_000 },
    async (t) => {
      configureEnvironment(t, apiOrigin);
      const controller = new AbortController();
      let notifyFetch: (() => void) | undefined;
      const started = new Promise<void>((resolve) => {
        notifyFetch = resolve;
      });
      const fetchMock = t.mock.method(
        globalThis,
        "fetch",
        async (_input: RequestInfo | URL, options?: RequestInit) => {
          const signal = options?.signal;
          assert.ok(signal instanceof AbortSignal);
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
      const pending = proxyProjects(
        localRequest({ signal: controller.signal }),
      );
      await started;
      controller.abort(new Error("private-api request canceled"));
      await assertError(await pending, 503);
      assert.equal(fetchMock.mock.callCount(), 1);
    },
  );

  test(
    "keeps the four-second timeout active while reading the upstream body",
    { timeout: 8_000 },
    async (t) => {
      let requestCount = 0;
      const server = createServer((_request, response) => {
        requestCount += 1;
        response.writeHead(200, { "Content-Type": "application/json" });
        response.flushHeaders();
      });
      t.after(async () => {
        server.closeAllConnections();
        await new Promise<void>((resolve) => server.close(() => resolve()));
      });
      await new Promise<void>((resolve, reject) => {
        server.once("error", reject);
        server.listen(0, "127.0.0.1", resolve);
      });
      const address = server.address();
      assert.ok(address && typeof address !== "string");
      configureEnvironment(t, `http://127.0.0.1:${address.port}`);
      const started = performance.now();
      await assertError(await proxyProjects(localRequest()), 503);
      assert.equal(requestCount, 1);
      assert.ok(performance.now() - started >= 3_500);
    },
  );
});
