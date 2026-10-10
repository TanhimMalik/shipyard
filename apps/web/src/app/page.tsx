import { FolderOpen } from "lucide-react";
import { ApiHealth } from "../components/api-health";

export default function OverviewPage() {
  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-4">
        <h1 className="text-2xl font-semibold tracking-tight">Overview</h1>
        <ApiHealth />
      </div>

      <section
        aria-labelledby="welcome-heading"
        className="rounded-lg border border-border bg-card px-6 py-8"
      >
        <div className="mb-4 flex size-10 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <FolderOpen size={20} aria-hidden="true" />
        </div>
        <h2 id="welcome-heading" className="text-base font-medium">
          Welcome to Shipyard
        </h2>
        <p className="mt-2 max-w-md text-sm leading-6 text-muted-foreground">
          Your projects and deployment activity will appear here.
        </p>
      </section>
    </div>
  );
}
