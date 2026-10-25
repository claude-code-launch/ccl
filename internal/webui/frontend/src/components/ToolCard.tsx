import { memo, useState } from "react";
import type { ReactNode } from "react";
import { ChevronRight, CircleX, LoaderCircle } from "lucide-react";
import type { ThreadItem, ToolItem } from "../thread";
import { editStats, isAgent, latestStep, toolActive, toolEdit, toolPlan, toolTodos } from "../thread";
import { useI18n } from "../i18n";
import { DiffStat, DiffView, PlanCard, TodoCard } from "./ToolViews";

// A tool call reads like claude.ai/code: one muted line — the call's own
// description, or the tool and its argument — that opens to what it did.
// Todo lists and plans are shown in full; edits open to their diff; a Task
// opens to what its sub-agent did, and shows its latest step while it works.
export const ToolCard = memo(function ToolCard({ tool, renderItems }: { tool: ToolItem; renderItems: (items: ThreadItem[]) => ReactNode }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const todos = toolTodos(tool);
  if (todos) return <TodoCard todos={todos} />;
  const plan = toolPlan(tool);
  if (plan) return <PlanCard plan={plan} />;

  const running = toolActive(tool);
  const failed = tool.status === "err" || tool.task?.status === "failed";
  const stopped = !running && tool.task?.status === "stopped";
  const edits = toolEdit(tool);
  const stats = editStats(tool);
  const line = tool.description || [tool.name, tool.summary].filter(Boolean).join(" ");
  const shell = tool.name === "Bash" || tool.name === "BashOutput";
  const agent = isAgent(tool);
  const step = agent && running && !open ? latestStep(tool) : "";
  const seconds = !running && tool.task?.usage?.durationMs ? `${(tool.task.usage.durationMs / 1000).toFixed(1)}s` : "";
  // A launched task's tool_result is a note for the model ("Async agent
  // launched…"); the work itself shows through its steps and summary.
  const output = tool.task && agent ? undefined : tool.output;
  return (
    <div className={"my-0.5 " + (tool.subagent ? "ml-5" : "")}>
      <details className="group/tool" onToggle={(event) => setOpen(event.currentTarget.open)}>
        <summary className="flex w-fit max-w-full cursor-pointer list-none items-center gap-1.5 py-0.5 pl-1 text-[14px] leading-5 text-ink-3 hover:text-ink-2 [&::-webkit-details-marker]:hidden">
          {running && <LoaderCircle size={13} className="shrink-0 animate-spin" aria-label={t("toolRunning")} />}
          {failed && <CircleX size={13} className="shrink-0 text-red-500" aria-label={t("toolFailed")} />}
          <span className="truncate" title={line}>
            {line}
          </span>
          {stats && <DiffStat {...stats} />}
          {agent && tool.children.length > 0 && <span className="shrink-0 text-[12px]">· {t("steps", { n: tool.children.length })}</span>}
          {seconds && <span className="shrink-0 text-[12px]">· {seconds}</span>}
          {stopped && <span className="shrink-0 text-[12px]">· {t("stopped")}</span>}
          <ChevronRight size={14} className="shrink-0 transition [details[open]>summary>&]:rotate-90" />
        </summary>
        <div className="mt-1.5 space-y-2 rounded-[10px] border border-line px-2 pb-2 pt-1.5">
          <div className="px-1 text-[13px] text-ink-2">{tool.name}</div>
          {edits ? (
            edits.map((edit, index) => <DiffView key={index} {...edit} />)
          ) : (
            tool.summary &&
            !agent && (
              <pre className="whitespace-pre-wrap break-words rounded-lg border border-line bg-code px-2.5 py-2 font-mono text-[12.5px] leading-[1.45] text-ink">
                {shell && <span className="select-none text-ink-3">$ </span>}
                {shell ? String(tool.input?.command || tool.summary) : tool.summary}
              </pre>
            )
          )}
          {agent && tool.children.length > 0 && <div className="border-l-2 border-line pl-3">{renderItems(tool.children)}</div>}
          {tool.task?.summary && !agent && !running && <div className="px-1 text-[12.5px] text-ink-3">{tool.task.summary}</div>}
          {output !== undefined && !edits && (
            <pre
              className={
                "max-h-[360px] overflow-auto whitespace-pre-wrap break-words px-1 font-mono text-[12.5px] leading-[1.45] " +
                (failed ? "text-red-600 dark:text-red-400" : "text-ink-3")
              }
            >
              {output || t("noOutput")}
            </pre>
          )}
          {edits && failed && tool.output && <pre className="whitespace-pre-wrap px-1 font-mono text-[12.5px] text-red-600 dark:text-red-400">{tool.output}</pre>}
          {output === undefined && running && !(agent && tool.children.length > 0) && (
            <div className="flex items-center gap-1.5 px-1 text-[12.5px] text-ink-3">
              <LoaderCircle size={12} className="animate-spin" /> {t("toolRunning")}
            </div>
          )}
        </div>
      </details>
      {step && (
        <div className="truncate pl-6 text-[13px] leading-5 text-ink-3" title={step}>
          {step}
        </div>
      )}
    </div>
  );
});
