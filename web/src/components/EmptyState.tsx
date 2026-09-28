import type { ReactNode } from "react";
import s from "./EmptyState.module.css";

/**
 * What a panel says when it has nothing in it.
 *
 * A sentence, not a dash and not a spinner. A spinner says something is coming, and if
 * nothing is coming a spinner is a lie told over and over. An empty state is the one
 * place where saying what is true and what to do about it is the whole of the design.
 */
export function EmptyState({
  icon,
  title,
  hint,
}: {
  icon?: ReactNode;
  title: string;
  hint?: string;
}) {
  return (
    <div className={s.empty}>
      {icon ? <div className={s.icon}>{icon}</div> : null}
      <h2 className={s.title}>{title}</h2>
      {hint ? <p className={s.hint}>{hint}</p> : null}
    </div>
  );
}
