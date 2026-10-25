import { useEffect, useState } from "react";
import { ArrowLeft, Bot, CircleX, LoaderCircle } from "lucide-react";
import type { ThreadItem, ToolItem } from "../thread";
import { agentsOf, toolActive } from "../thread";
import { elapsed, tokenCount } from "../format";
import { useI18n } from "../i18n";

interface Props {
  items: ThreadItem[];
  // The main conversation is working.
  running: boolean;
  // The sub-agent the main view shows (its call's key), or null for main.
  selected: string | null;
  onSelect: (key: string | null) => void;
}

// The sub-agents of a conversation, like Claude Code's agent list: the main
// thread, then each sub-agent with its type, what it was asked, how long it
// has run and what it spent. Selecting one shows its conversation.
export function AgentsPanel({ items, running, selected, onSelect }: Props) {
  const { t } = useI18n();
  const agents = agentsOf(items);
  const now = useClock(agents.some(({ tool }) => toolActive(tool)));
  return (
    <aside className="min-h-0 flex-1 overflow-y-auto px-2 pb-3 pt-1">
      <div className="px-2 pb-1.5 pt-1 text-[13px] font-medium text-ink-2">{t("agents")}</div>
      <button
        type="button"
        onClick={() => onSelect(null)}
        className={"flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left text-[14px] text-ink hover:bg-hover " + (selected === null ? "bg-active" : "")}
      >
        <Dot running={running} />
        <span className="font-medium">main</span>
      </button>
      {!agents.length && <p className="px-2 py-3 text-[13px] text-ink-3">{t("noAgents")}</p>}
      {agents.map(({ tool, depth }) => (
        <button
          key={tool.key}
          type="button"
          onClick={() => onSelect(tool.key)}
          style={{ marginLeft: depth * 16, width: `calc(100% - ${depth * 16}px)` }}
          className={"flex items-start gap-2 rounded-lg px-2 py-1.5 text-left hover:bg-hover " + (selected === tool.key ? "bg-active" : "")}
        >
          <span className="mt-[3px]">{tool.task?.status === "failed" ? <CircleX size={13} className="text-red-500" aria-label={t("toolFailed")} /> : <Dot running={toolActive(tool)} />}</span>
          <AgentLabel tool={tool} now={now} />
        </button>
      ))}
    </aside>
  );
}

// AgentBanner heads the main view while it shows a sub-agent.
export function AgentBanner({ tool, onBack }: { tool: ToolItem; onBack: () => void }) {
  const { t } = useI18n();
  const now = useClock(toolActive(tool));
  return (
    <div className="mx-auto mt-2 flex w-full max-w-[691px] items-start gap-2 pl-6 pr-8 max-sm:px-4">
      <button type="button" onClick={onBack} className="flex shrink-0 items-center gap-1 rounded-lg px-2 py-1 text-[13px] text-ink-2 hover:bg-hover hover:text-ink">
        <ArrowLeft size={14} /> main
      </button>
      <div className="flex min-w-0 flex-1 items-start gap-2 rounded-lg bg-hover px-2.5 py-1.5">
        <span className="mt-[2px]">{toolActive(tool) ? <LoaderCircle size={14} className="animate-spin text-clay" aria-label={t("toolRunning")} /> : <Bot size={14} className="text-ink-3" />}</span>
        <AgentLabel tool={tool} now={now} />
      </div>
    </div>
  );
}

function AgentLabel({ tool, now }: { tool: ToolItem; now: number }) {
  const { t } = useI18n();
  const running = toolActive(tool);
  const status = tool.task?.status;
  const type = tool.task?.agent || String(tool.input?.subagent_type || "") || tool.name;
  const what = tool.description || tool.task?.description || tool.summary;
  const spent = tool.task?.usage?.durationMs || (tool.startedAt ? (tool.endedAt || (running ? now : tool.startedAt)) - tool.startedAt : 0);
  const parts = [
    running ? t("toolRunning") : status === "stopped" ? t("stopped") : status === "failed" ? t("failed") : t("done"),
    spent > 0 ? elapsed(spent) : "",
    tool.task?.usage?.tokens ? `↓ ${tokenCount(tool.task.usage.tokens)} tokens` : "",
    tool.children.length ? t("steps", { n: tool.children.length }) : "",
  ].filter(Boolean);
  return (
    <span className="min-w-0 flex-1">
      <span className="flex items-baseline gap-2 text-[14px]">
        <span className="shrink-0 text-ink">{type}</span>
        <span className="truncate text-ink-2" title={what}>
          {what}
        </span>
      </span>
      <span className="block text-[12px] text-ink-3">{parts.join(" · ")}</span>
    </span>
  );
}

// useClock ticks every second while something is running, for live durations.
function useClock(live: boolean): number {
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (!live) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [live]);
  return now;
}

function Dot({ running }: { running: boolean }) {
  return running ? <LoaderCircle size={13} className="shrink-0 animate-spin text-clay" /> : <span className="mx-[3px] block size-[7px] shrink-0 rounded-full border border-ink-3" />;
}
