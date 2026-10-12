export const projectFields = [
  "name",
  "slug",
  "git_url",
  "branch",
  "app_port",
  "health_path",
] as const;

export type ProjectField = (typeof projectFields)[number];

export type ProjectInput = {
  name: string;
  slug: string;
  git_url: string;
  branch: string;
  app_port: number;
  health_path: string;
};

export type Project = ProjectInput & {
  id: string;
  created_at: string;
  updated_at: string;
};

export type ProjectPage = {
  projects: Project[];
  next_offset: number | null;
};

export const projectIDPattern =
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function isProject(value: unknown): value is Project {
  if (typeof value !== "object" || value === null) return false;
  const project = value as Record<string, unknown>;
  if (
    typeof project.id !== "string" ||
    !projectIDPattern.test(project.id) ||
    typeof project.app_port !== "number" ||
    !Number.isInteger(project.app_port) ||
    project.app_port < 1 ||
    project.app_port > 65535 ||
    !["name", "slug", "git_url", "branch", "health_path"].every(
      (field) =>
        typeof project[field] === "string" && project[field].length > 0,
    ) ||
    !["created_at", "updated_at"].every(
      (field) =>
        typeof project[field] === "string" &&
        Number.isFinite(Date.parse(project[field])),
    )
  ) {
    return false;
  }
  try {
    const repository = new URL(project.git_url as string);
    return (
      repository.protocol === "https:" &&
      !repository.username &&
      !repository.password &&
      !repository.search &&
      !repository.hash
    );
  } catch {
    return false;
  }
}

export function isProjectPage(value: unknown): value is ProjectPage {
  if (typeof value !== "object" || value === null) return false;
  const page = value as Record<string, unknown>;
  return (
    Array.isArray(page.projects) &&
    page.projects.length <= 100 &&
    page.projects.every(isProject) &&
    (page.next_offset === null ||
      (typeof page.next_offset === "number" &&
        Number.isSafeInteger(page.next_offset) &&
        page.next_offset > 0 &&
        page.projects.length > 0))
  );
}
