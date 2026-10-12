"use client";

import { useId, type InputHTMLAttributes } from "react";
import type { ProjectField as ProjectFieldName } from "../../lib/projects.ts";

type ProjectFieldProps = Omit<
  InputHTMLAttributes<HTMLInputElement>,
  "id" | "name" | "aria-invalid" | "aria-describedby"
> & {
  name: ProjectFieldName;
  label: string;
  hint?: string;
  error?: string;
};

export function ProjectField({
  label,
  hint,
  error,
  className = "",
  ...inputProps
}: ProjectFieldProps) {
  const id = useId();
  const hintId = `${id}-hint`;
  const errorId = `${id}-error`;
  const description = [hint ? hintId : "", error ? errorId : ""]
    .filter(Boolean)
    .join(" ");

  return (
    <div className="min-w-0 space-y-2">
      <label htmlFor={id} className="block text-sm font-medium">
        {label}
      </label>
      <input
        {...inputProps}
        id={id}
        aria-invalid={error ? true : undefined}
        aria-describedby={description || undefined}
        className={`min-h-11 w-full min-w-0 rounded-lg border bg-background px-3 py-2 text-sm text-foreground placeholder:text-muted-foreground disabled:cursor-wait disabled:opacity-60 ${error ? "border-danger" : "border-border-strong"} ${className}`}
      />
      {hint ? (
        <p id={hintId} className="text-xs leading-5 text-secondary-foreground">
          {hint}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} className="text-sm text-danger">
          {error}
        </p>
      ) : null}
    </div>
  );
}
