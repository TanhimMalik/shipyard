export const dynamic = "force-dynamic";
export const runtime = "nodejs";

export async function GET(request: Request) {
  let available = false;

  try {
    const configuredOrigin =
      process.env.SHIPYARD_API_URL ??
      (process.env.NODE_ENV === "production" ? "" : "http://127.0.0.1:8080");
    const origin = new URL(configuredOrigin);

    if (
      !["http:", "https:"].includes(origin.protocol) ||
      (process.env.VERCEL === "1" && origin.protocol !== "https:") ||
      origin.username ||
      origin.password ||
      origin.search ||
      origin.hash ||
      origin.pathname !== "/" ||
      origin.origin === new URL(request.url).origin
    ) {
      throw new Error("Invalid API origin");
    }

    const response = await fetch(new URL("/api/health", origin), {
      headers: { Accept: "application/json" },
      cache: "no-store",
      redirect: "error",
      signal: AbortSignal.any([request.signal, AbortSignal.timeout(3000)]),
    });

    if (response.status === 200) {
      const body: unknown = await response.json();
      available =
        typeof body === "object" &&
        body !== null &&
        "status" in body &&
        body.status === "ok";
    } else {
      await response.body?.cancel();
    }
  } catch {
    available = false;
  }

  return Response.json(
    { status: available ? "ok" : "unavailable" },
    {
      status: available ? 200 : 503,
      headers: { "Cache-Control": "no-store" },
    },
  );
}
