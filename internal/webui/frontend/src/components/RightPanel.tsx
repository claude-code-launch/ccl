import { useState } from "react";
import type { ReactNode } from "react";
import { ChevronDown, Cloud, Cpu, FolderGit2, Gauge, GitBranch } from "lucide-react";
import type { Capabilities, GitStatus, ProviderView, Session } from "../types";
import { DiffStat } from "./ToolViews";
import { basename, money } from "../format";
import { useI18n } from "../i18n";

interface Props {
  session: Session | null;
  providers: ProviderView[];
  workspace: string;
  capabilities: Capabilities;
  git: GitStatus | null;
  onOpenChange?: (path: string) => void;
}

// The side panel mirrors claude.ai/code's environment / context / changes
// cards with what a ccl session has: where it runs, what it spent, and the
// connectors Claude Code loaded.
export function RightPanel({ session, providers, workspace, capabilities, git, onOpenChange }: Props) {
  const { t } = useI18n();
  const provider = providers.find((item) => session && item.name === session.provider);
  const model = session?.model ? provider?.modelChoices.find((choice) => choice.value === session.model)?.label || session.model : t("auto");
  const usage = session?.usage;
  return (
    <aside className="w-[290px] shrink-0 space-y-2 overflow-y-auto px-2 pb-3 pt-1">
      <Card title={t("environment")}>
        <Row icon={<Cloud size={15} />} label={session?.provider || "default"} />
        <Row icon={<FolderGit2 size={15} />} label={basename(workspace) || "."} title={workspace} />
        <Row icon={<Cpu size={15} />} label={model} />
        {session?.effort && <Row icon={<Gauge size={15} />} label={session.effort} />}
        {git?.repo && git.branch && <Row icon={<GitBranch size={15} />} label={git.branch + (git.ahead ? ` ↑${git.ahead}` : "") + (git.behind ? ` ↓${git.behind}` : "")} />}
      </Card>
      {git?.repo && (
        <Card title={t("changes")}>
          {!git.files.length && <p className="px-1 py-1 text-[13px] text-ink-3">{t("noChanges")}</p>}
          {git.files.length > 0 && (
            <div className="flex items-center gap-2 px-1 py-1 text-[13px] text-ink-2">
              {t("filesChanged", { n: git.files.length })}
              <span className="ml-auto"><DiffStat added={git.added} deleted={git.deleted} /></span>
            </div>
          )}
          {git.files.slice(0, 8).map((file) => (
            <button
              key={file.path}
              onClick={() => onOpenChange?.(file.path)}
              title={file.path}
              className="flex w-full items-center gap-2 rounded-md px-1 py-1 text-left text-[13px] text-ink hover:bg-hover"
            >
              <span className={"w-3 shrink-0 font-mono text-[11px] " + (file.untracked ? "text-emerald-500" : file.conflicted ? "text-red-500" : "text-amber-500")}>
                {file.untracked ? "U" : file.conflicted ? "!" : (file.worktree !== "." ? file.worktree : file.index)}
              </span>
              <span className="min-w-0 flex-1 truncate">{file.path}</span>
              <DiffStat added={file.added} deleted={file.deleted} />
            </button>
          ))}
          {git.files.length > 8 && <p className="px-1 pt-0.5 text-xs text-ink-3">{t("moreFiles", { n: git.files.length - 8 })}</p>}
        </Card>
      )}
      <Card title={t("usageTitle")}>
        <Stat label={t("costTotal")} value={usage?.costUsd ? money(usage.costUsd) : "—"} />
        <Stat label={t("outputTokens")} value={usage?.outputTokens ? usage.outputTokens.toLocaleString() : "—"} />
        <Stat label={t("turnsLabel")} value={String(session?.turns || 0)} />
        <Stat label={t("claudeSession")} value={session?.claudeSessionId ? session.claudeSessionId.slice(0, 8) : t("notStarted")} title={session?.claudeSessionId} mono />
      </Card>
      <Card title={t("connectorsTitle")}>
        {!capabilities.mcpServers.length && <p className="px-1 py-1 text-[13px] text-ink-3">{t("noConnectors")}</p>}
        {capabilities.mcpServers.map((server) => (
          <div key={server.name} className="flex items-center gap-2 px-1 py-1 text-[14px]">
            <span className={"size-2 shrink-0 rounded-full " + (server.status === "connected" ? "bg-emerald-500" : server.status === "failed" ? "bg-red-500" : "bg-amber-500")} />
            <span className="truncate text-ink">{server.name}</span>
            <span className="ml-auto text-xs text-ink-3">{server.status}</span>
          </div>
        ))}
      </Card>
    </aside>
  );
}

function Card({ title, children }: { title: string; children: ReactNode }) {
  const [open, setOpen] = useState(true);
  return (
    <section className="rounded-[10px] border border-line px-2 py-1.5">
      <button onClick={() => setOpen(!open)} className="flex w-full items-center gap-1 px-1 py-1 text-[13px] text-ink-2 hover:text-ink">
        {title}
        <ChevronDown size={13} className={"text-ink-3 transition " + (open ? "" : "-rotate-90")} />
      </button>
      {open && <div className="pb-0.5">{children}</div>}
    </section>
  );
}

function Row({ icon, label, title }: { icon: ReactNode; label: string; title?: string }) {
  return (
    <div title={title || label} className="flex items-center gap-2.5 px-1 py-1 text-[14px] text-ink">
      <span className="shrink-0 text-ink-3">{icon}</span>
      <span className="truncate">{label}</span>
    </div>
  );
}

function Stat({ label, value, title, mono }: { label: string; value: string; title?: string; mono?: boolean }) {
  return (
    <div title={title} className="flex items-center justify-between gap-3 px-1 py-1 text-[13px]">
      <span className="text-ink-3">{label}</span>
      <span className={"truncate text-ink " + (mono ? "font-mono text-[12px]" : "")}>{value}</span>
    </div>
  );
}
