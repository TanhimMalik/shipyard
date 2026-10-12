import { proxyProjects } from "../../../lib/server/projects.ts";

export const dynamic = "force-dynamic";
export const runtime = "nodejs";

export function GET(request: Request) {
  return proxyProjects(request);
}

export function POST(request: Request) {
  return proxyProjects(request);
}
