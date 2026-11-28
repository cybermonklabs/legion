import type { ReactNode } from "react";

export default function Dashboard({ name }: { name: string }): ReactNode {
  return <p>{name}</p>;
}
