import type { ReactNode } from "react";
import { ThemeToggle } from "../theme-toggle";
import { MobileNavigation } from "./mobile-navigation";
import { SidebarContent } from "./sidebar-content";
import { ShellBreadcrumb } from "./shell-breadcrumb";

export function AppShell({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-dvh md:grid md:grid-cols-[var(--sidebar-width)_minmax(0,1fr)]">
      <a
        href="#main-content"
        className="sr-only z-50 rounded-lg bg-primary px-4 py-3 font-medium text-primary-foreground focus:not-sr-only focus:fixed focus:top-3 focus:left-3"
      >
        Skip to content
      </a>

      <aside className="sticky top-0 hidden h-dvh border-r border-border bg-sidebar text-sidebar-foreground md:block">
        <SidebarContent />
      </aside>

      <div className="min-w-0">
        <header className="flex h-16 items-center justify-between gap-3 border-b border-border px-4 md:px-6">
          <div className="flex min-w-0 items-center gap-2 text-sm">
            <MobileNavigation />
            <ShellBreadcrumb />
          </div>
          <ThemeToggle />
        </header>

        <main
          id="main-content"
          tabIndex={-1}
          className="mx-auto max-w-7xl p-4 md:p-6"
        >
          {children}
        </main>
      </div>
    </div>
  );
}
