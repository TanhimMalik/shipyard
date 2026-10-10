"use client";

import { CircleAlert, CircleCheck, LoaderCircle } from "lucide-react";
import { useEffect, useState } from "react";

type HealthState = "checking" | "available" | "unavailable";

const states = {
  checking: {
    label: "Checking Go API",
    icon: LoaderCircle,
    iconClass: "animate-spin text-muted-foreground motion-reduce:animate-none",
  },
  available: {
    label: "Go API available",
    icon: CircleCheck,
    iconClass: "text-success",
  },
  unavailable: {
    label: "Go API unavailable",
    icon: CircleAlert,
    iconClass: "text-danger",
  },
};

export function ApiHealth() {
  const [health, setHealth] = useState<HealthState>("checking");
  const [attempt, setAttempt] = useState(0);
  const { label, icon: Icon, iconClass } = states[health];

  useEffect(() => {
    let active = true;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 5000);

    async function check() {
      let available = false;

      try {
        const response = await fetch("/api/health", {
          cache: "no-store",
          signal: controller.signal,
        });
        if (response.status === 200) {
          const body: unknown = await response.json();
          available =
            typeof body === "object" &&
            body !== null &&
            "status" in body &&
            body.status === "ok";
        }
      } catch {
        available = false;
      } finally {
        window.clearTimeout(timeout);
      }

      if (active) {
        setHealth(available ? "available" : "unavailable");
      }
    }

    void check();
    return () => {
      active = false;
      window.clearTimeout(timeout);
      controller.abort();
    };
  }, [attempt]);

  return (
    <div className="flex flex-wrap items-center gap-2 rounded-lg border border-border bg-card py-1 pl-3 text-sm text-secondary-foreground">
      <p role="status" aria-atomic="true" className="flex items-center gap-2">
        <Icon size={16} className={iconClass} aria-hidden="true" />
        {label}
      </p>
      <button
        type="button"
        aria-label="Check Go API again"
        disabled={health === "checking"}
        onClick={() => {
          setHealth("checking");
          setAttempt((current) => current + 1);
        }}
        className="flex min-h-11 items-center rounded-lg px-3 font-medium hover:bg-muted disabled:cursor-wait disabled:opacity-50"
      >
        Check again
      </button>
    </div>
  );
}
