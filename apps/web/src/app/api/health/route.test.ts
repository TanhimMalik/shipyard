import assert from "node:assert/strict";
import { createServer } from "node:http";
import { describe, test, type TestContext } from "node:test";
import { GET } from "./route.ts";

const requestUrl = "http://127.0.0.1:3000/api/health";

function configureEnvironment(
  t: TestContext,
  apiUrl: string | undefined,
  nodeEnv = "production",
) {
  const previous = {
    NODE_ENV: process.env.NODE_ENV,
    SHIPYARD_API_URL: process.env.SHIPYARD_API_URL,
  };

  t.after(() => {
    for (const [key, value] of Object.entries(previous)) {
      if (value === undefined) Reflect.deleteProperty(process.env, key);
      else Reflect.set(process.env, key, value);
    }
  });

  Reflect.set(process.env, "NODE_ENV", nodeEnv);
  if (apiUrl === undefined) {
    Reflect.deleteProperty(process.env, "SHIPYARD_API_URL");
  } else {
    process.env.SHIPYARD_API_URL = apiUrl;
  }
}

async function assertUnavailable(response: Response) {
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  assert.deepEqual(await response.json(), { status: "unavailable" });
}

describe("health proxy", { concurrency: false }, () => {
  test("returns a fresh healthy result from the configured API", async (t) => {
    configureEnvironment(t, "https://api.example.test/");
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (input: RequestInfo | URL, options?: RequestInit) => {
        assert.equal(String(input), "https://api.example.test/api/health");
        assert.equal(options?.cache, "no-store");
        assert.equal(options?.redirect, "error");
        assert.ok(options?.signal instanceof AbortSignal);
        assert.equal(options.signal.aborted, false);
        return Response.json({ status: "ok", internal: "not for clients" });
      },
    );

    const response = await GET(new Request(requestUrl));

    assert.equal(fetchMock.mock.callCount(), 1);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("Cache-Control"), "no-store");
    assert.deepEqual(await response.json(), { status: "ok" });
  });

  test("does not trust healthy JSON from a non-200 response", async (t) => {
    for (const status of [201, 302, 503]) {
      await t.test(`upstream status ${status}`, async (t) => {
        configureEnvironment(t, "https://api.example.test");
        t.mock.method(globalThis, "fetch", async () =>
          Response.json({ status: "ok" }, { status }),
        );
        await assertUnavailable(await GET(new Request(requestUrl)));
      });
    }
  });

  test("rejects malformed JSON and unexpected health payloads", async (t) => {
    const bodies = ["{", "null", "[]", "{}", '{"status":"unavailable"}'];
    for (const body of bodies) {
      await t.test(body, async (t) => {
        configureEnvironment(t, "https://api.example.test");
        t.mock.method(globalThis, "fetch", async () => new Response(body));
        await assertUnavailable(await GET(new Request(requestUrl)));
      });
    }
  });

  test("hides network and redirect failures", async (t) => {
    const errors = [
      new Error("connection refused at https://private-api.example.test"),
      new TypeError("fetch failed", {
        cause: new Error("unexpected redirect to https://private.example.test"),
      }),
    ];
    for (const [index, error] of errors.entries()) {
      await t.test(`failure ${index + 1}`, async (t) => {
        configureEnvironment(t, "https://api.example.test");
        t.mock.method(globalThis, "fetch", async () => {
          throw error;
        });
        await assertUnavailable(await GET(new Request(requestUrl)));
      });
    }
  });

  test("uses the loopback default outside production", async (t) => {
    configureEnvironment(t, undefined, "development");
    const fetchMock = t.mock.method(
      globalThis,
      "fetch",
      async (input: RequestInfo | URL) => {
        assert.equal(String(input), "http://127.0.0.1:8080/api/health");
        return Response.json({ status: "ok" });
      },
    );
    const response = await GET(new Request(requestUrl));
    assert.equal(fetchMock.mock.callCount(), 1);
    assert.equal(response.status, 200);
  });

  test("does not replace an empty development setting with the default", async (t) => {
    configureEnvironment(t, "", "development");
    const fetchMock = t.mock.method(globalThis, "fetch", async () => {
      throw new Error("unexpected upstream request");
    });
    await assertUnavailable(await GET(new Request(requestUrl)));
    assert.equal(fetchMock.mock.callCount(), 0);
  });

  test("requires an API origin in production", async (t) => {
    for (const value of [undefined, ""]) {
      await t.test(value === undefined ? "missing" : "empty", async (t) => {
        configureEnvironment(t, value);
        const fetchMock = t.mock.method(globalThis, "fetch", async () => {
          throw new Error("unexpected upstream request");
        });
        await assertUnavailable(await GET(new Request(requestUrl)));
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("rejects values that are not plain HTTP origins", async (t) => {
    const invalidOrigins = [
      "not a URL",
      "ftp://api.example.test",
      "https://user:password@api.example.test",
      "https://api.example.test/other-path",
      "https://api.example.test?token=secret",
      "https://api.example.test#fragment",
    ];
    for (const [index, origin] of invalidOrigins.entries()) {
      await t.test(`invalid origin ${index + 1}`, async (t) => {
        configureEnvironment(t, origin);
        const fetchMock = t.mock.method(globalThis, "fetch", async () => {
          throw new Error("unexpected upstream request");
        });
        await assertUnavailable(await GET(new Request(requestUrl)));
        assert.equal(fetchMock.mock.callCount(), 0);
      });
    }
  });

  test("rejects its own origin before fetching", async (t) => {
    configureEnvironment(t, "http://127.0.0.1:3000/");
    const fetchMock = t.mock.method(globalThis, "fetch", async () => {
      throw new Error("unexpected recursive request");
    });

    await assertUnavailable(await GET(new Request(requestUrl)));
    assert.equal(fetchMock.mock.callCount(), 0);
  });

  test(
    "keeps the timeout active while reading the body",
    { timeout: 8_000 },
    async (t) => {
      let requestCount = 0;
      const server = createServer((_request, response) => {
        requestCount += 1;
        response.writeHead(200, { "Content-Type": "application/json" });
        response.flushHeaders();
        // A peer can send headers promptly and then never finish its body.
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
      await assertUnavailable(await GET(new Request(requestUrl)));

      assert.equal(requestCount, 1);
      assert.ok(performance.now() - started >= 2_500);
    },
  );
});
