import { ArrowLeft } from "lucide-react";
import type { Metadata } from "next";
import Link from "next/link";
import { CreateProjectForm } from "../../../components/projects/create-project-form";

export const metadata: Metadata = { title: "New project · Shipyard" };

export default function NewProjectPage() {
  return (
    <div className="max-w-3xl space-y-6">
      <Link
        href="/projects"
        className="inline-flex min-h-11 items-center gap-2 rounded-lg text-sm font-medium text-secondary-foreground hover:text-foreground"
      >
        <ArrowLeft size={16} aria-hidden="true" />
        Back to projects
      </Link>
      <div>
        <h1 className="text-2xl font-semibold tracking-tight">New project</h1>
        <p className="mt-2 text-sm text-muted-foreground">
          Save the repository and runtime settings for your application.
        </p>
      </div>
      <CreateProjectForm />
    </div>
  );
}
