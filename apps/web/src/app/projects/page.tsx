import type { Metadata } from "next";
import { ProjectDirectory } from "../../components/projects/project-directory";

export const metadata: Metadata = { title: "Projects · Shipyard" };

export default function ProjectsPage() {
  return <ProjectDirectory />;
}
