import { memo, useMemo } from "react";
import { CircleCheck, Circle, LoaderCircle, ListTodo, Map as MapIcon } from "lucide-react";
import { diffLines, withContext } from "../diff";
import type { Todo } from "../thread";
import { useI18n } from "../i18n";
import { Markdown } from "./Markdown";

export function DiffStat({ added, deleted }: { added: number; deleted: number }) {
  if (!added && !deleted) return null;
  return (
    <span className="inline-flex shrink-0 items-center gap-1 rounded px-1 font-mono text-[11.5px] leading-4">
      {added > 0 && <span className="text-emerald-600 dark:text-emerald-400">+{added}</span>}
      {deleted > 0 && <span className="text-red-600 dark:text-red-400">−{deleted}</span>}
    </span>
  );
}

// DiffView shows one edit with three lines of context around each change.
export const DiffView = memo(function DiffView({ path, before, after }: { path: string; before: string; after: string }) {
  const { t } = useI18n();
  const rows = useMemo(() => withContext(diffLines(before, after)), [before, after]);
  return (
    <div className="overflow-hidden rounded-lg border border-line">
      {path && <div className="truncate border-b border-line bg-code px-2.5 py-1 font-mono text-[12px] text-ink-2" title={path}>{path}</div>}
      <div className="max-h-[420px] overflow-auto font-mono text-[12.5px] leading-[1.45]">
        {rows.map((row, index) =>
          row.type === "gap" ? (
            <div key={index} className="bg-code px-2.5 py-0.5 text-[11.5px] text-ink-3">{t("unchangedLines", { n: row.count })}</div>
          ) : (
            <div
              key={index}
              className={
                "flex whitespace-pre-wrap break-all px-2.5 " +
                (row.type === "add"
                  ? "bg-emerald-500/10 text-emerald-800 dark:text-emerald-200"
                  : row.type === "del"
                    ? "bg-red-500/10 text-red-800 dark:text-red-200"
                    : "text-ink-2")
              }
            >
              <span className="w-4 shrink-0 select-none text-ink-3">{row.type === "add" ? "+" : row.type === "del" ? "−" : " "}</span>
              <span>{row.text || " "}</span>
            </div>
          ),
        )}
      </div>
    </div>
  );
});

// TodoCard is the latest TodoWrite list, always visible like claude.ai/code's.
export function TodoCard({ todos }: { todos: Todo[] }) {
  const { t } = useI18n();
  const done = todos.filter((todo) => todo.status === "completed").length;
  return (
    <div className="my-1 rounded-[10px] border border-line px-3 py-2">
      <div className="mb-1.5 flex items-center gap-2 text-[13px] text-ink-2">
        <ListTodo size={14} className="text-ink-3" />
        {t("todos")}
        <span className="text-ink-3">{done}/{todos.length}</span>
      </div>
      <ul className="space-y-1">
        {todos.map((todo, index) => (
          <li key={index} className="flex items-start gap-2 text-[14px] leading-5">
            {todo.status === "completed" ? (
              <CircleCheck size={15} className="mt-0.5 shrink-0 text-emerald-600 dark:text-emerald-400" />
            ) : todo.status === "in_progress" ? (
              <LoaderCircle size={15} className="mt-0.5 shrink-0 animate-spin text-clay" />
            ) : (
              <Circle size={15} className="mt-0.5 shrink-0 text-ink-3" />
            )}
            <span className={todo.status === "completed" ? "text-ink-3 line-through" : todo.status === "in_progress" ? "text-ink" : "text-ink-2"}>
              {todo.status === "in_progress" && todo.activeForm ? todo.activeForm : todo.content}
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

export function PlanCard({ plan }: { plan: string }) {
  const { t } = useI18n();
  return (
    <div className="my-1 rounded-[10px] border border-line px-3.5 py-2.5">
      <div className="mb-1 flex items-center gap-2 text-[13px] text-ink-2">
        <MapIcon size={14} className="text-ink-3" />
        {t("plan")}
      </div>
      <Markdown text={plan} />
    </div>
  );
}
