"use client";

import { usePathname } from "next/navigation";

export function ShellBreadcrumb() {
  const pathname = usePathname();
  const label =
    pathname === "/projects/new"
      ? "New project"
      : pathname.startsWith("/projects/")
        ? "Project"
        : pathname === "/projects"
          ? "Projects"
          : "Overview";

  return (
    <>
      <span className="text-secondary-foreground">Shipyard</span>
      <span aria-hidden="true" className="text-border-strong">
        /
      </span>
      <span>{label}</span>
    </>
  );
}
