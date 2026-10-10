import { LayoutDashboard } from "lucide-react";
import Link from "next/link";
import type { ReactNode } from "react";
import { BrandMark } from "../brand-mark";

export function SidebarContent({
  closeButton,
  onNavigate,
}: {
  closeButton?: ReactNode;
  onNavigate?: () => void;
}) {
  return (
    <>
      <div className="flex h-16 items-center justify-between border-b border-border px-4">
        <Link
          href="/"
          onClick={onNavigate}
          aria-label="Shipyard overview"
          className="flex items-center gap-2.5 rounded-sm font-semibold tracking-tight"
        >
          <BrandMark className="size-8" />
          <span>Shipyard</span>
        </Link>
        {closeButton}
      </div>

      <nav aria-label="Main navigation" className="p-3">
        <Link
          href="/"
          aria-current="page"
          onClick={onNavigate}
          className="flex min-h-11 items-center gap-3 rounded-lg bg-muted px-3 text-sm font-medium text-sidebar-foreground"
        >
          <LayoutDashboard size={18} aria-hidden="true" />
          Overview
        </Link>
      </nav>
    </>
  );
}
