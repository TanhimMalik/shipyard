"use client";

import { Moon, Sun } from "lucide-react";
import { useTheme } from "next-themes";

export function ThemeToggle() {
  const { setTheme } = useTheme();

  return (
    <button
      type="button"
      aria-label="Switch color theme"
      onClick={() => {
        setTheme(
          document.documentElement.classList.contains("dark")
            ? "light"
            : "dark",
        );
      }}
      className="flex size-11 items-center justify-center rounded-lg text-muted-foreground hover:bg-muted hover:text-foreground"
    >
      <Sun size={18} className="hidden dark:block" aria-hidden="true" />
      <Moon size={18} className="dark:hidden" aria-hidden="true" />
    </button>
  );
}
