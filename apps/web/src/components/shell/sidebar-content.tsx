"use client";

import { FolderOpen, LayoutDashboard } from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import type { ReactNode } from "react";
import { BrandMark } from "../brand-mark";

const links = [
  { href: "/", label: "Overview", icon: LayoutDashboard },
  { href: "/projects", label: "Projects", icon: FolderOpen },
];

export function SidebarContent({
  closeButton,
  onNavigate,
}: {
  closeButton?: ReactNode;
  onNavigate?: () => void;
}) {
  const pathname = usePathname();

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
      <nav aria-label="Main navigation" className="space-y-1 p-3">
        {links.map(({ href, label, icon: Icon }) => {
          const active =
            href === "/"
              ? pathname === href
              : pathname === href || pathname.startsWith(`${href}/`);
          return (
            <Link
              key={href}
              href={href}
              aria-current={active ? "page" : undefined}
              onClick={onNavigate}
              className={`flex min-h-11 items-center gap-3 rounded-lg px-3 text-sm font-medium ${active ? "bg-muted text-sidebar-foreground" : "text-muted-foreground hover:bg-muted hover:text-sidebar-foreground"}`}
            >
              <Icon size={18} aria-hidden="true" />
              {label}
            </Link>
          );
        })}
      </nav>
    </>
  );
}
