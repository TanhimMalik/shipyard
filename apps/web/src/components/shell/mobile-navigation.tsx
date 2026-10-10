"use client";

import { Menu, X } from "lucide-react";
import { useEffect, useRef } from "react";
import { SidebarContent } from "./sidebar-content";

export function MobileNavigation() {
  const dialog = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    const desktop = window.matchMedia("(min-width: 48rem)");
    function closeOnDesktop() {
      if (desktop.matches) dialog.current?.close();
    }
    desktop.addEventListener("change", closeOnDesktop);
    return () => desktop.removeEventListener("change", closeOnDesktop);
  }, []);

  function close() {
    dialog.current?.close();
  }

  return (
    <>
      <button
        type="button"
        aria-label="Open navigation"
        aria-haspopup="dialog"
        aria-controls="mobile-navigation"
        onClick={() => dialog.current?.showModal()}
        className="flex size-11 items-center justify-center rounded-lg text-muted-foreground hover:bg-muted hover:text-foreground md:hidden"
      >
        <Menu size={20} aria-hidden="true" />
      </button>

      <dialog
        id="mobile-navigation"
        ref={dialog}
        aria-label="Main navigation"
        onClick={(event) => {
          if (event.target === event.currentTarget) close();
        }}
        onKeyDown={(event) => {
          if (event.key === "Escape") close();
        }}
        className="fixed inset-y-0 left-0 m-0 h-dvh max-h-none w-(--sidebar-width) max-w-[calc(100%-2rem)] border-0 border-r border-border bg-sidebar p-0 text-sidebar-foreground"
      >
        <SidebarContent
          onNavigate={close}
          closeButton={
            <button
              type="button"
              aria-label="Close navigation"
              onClick={close}
              className="flex size-11 items-center justify-center rounded-lg text-muted-foreground hover:bg-muted hover:text-foreground"
            >
              <X size={20} aria-hidden="true" />
            </button>
          }
        />
      </dialog>
    </>
  );
}
