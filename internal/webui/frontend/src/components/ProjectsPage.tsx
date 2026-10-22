import { useEffect, useMemo, useState } from "react";
import type { ReactNode } from "react";
import { Archive, ArchiveRestore, ArrowLeft, Blocks, FolderGit2, GitBranch, FolderOpen, FolderPlus, History, LoaderCircle, MessageSquare, PanelLeft, Pin, Plus, Search, SlidersHorizontal, Trash2, X } from "lucide-react";
import type { Api } from "../api";
import type { ProjectDefaults, ProviderView, Session, Workspace } from "../types";
import { EFFORT_LEVELS, PERMISSION_MODES } from "../types";
import { relativeTime } from "../format";
import { useI18n } from "../i18n";
import { IconButton } from "./Sidebar";
import { MemoryEditor, projectMemorySources } from "./MemoryEditor";
import { PermissionRules } from "./PermissionRules";

interface Common {
  sidebarHidden: boolean;
  onShowSidebar: () => void;
}

// PageHeader is the title bar of a page outside a conversation. A phone
// always shows the sidebar button; the sidebar is an overlay there.
function PageHeader({ sidebarHidden, onShowSidebar, children }: Common & { children?: ReactNode }) {
  const { t } = useI18n();
  return (
    <header className="flex h-12 shrink-0 items-center gap-1 px-3">
      <span className={sidebarHidden ? "" : "md:hidden"}>
        <IconButton title={t("toggleSidebar")} onClick={onShowSidebar}>
          <PanelLeft size={17} />
        </IconButton>
      </span>
      {children}
    </header>
  );
}

const baseName = (path: string) => path.split(/[\\/]/).filter(Boolean).pop() || path;

// ProjectsList is the projects page: every folder the server offers, with
// how many conversations each has and when it was last used.
export function ProjectsList(props: Common & {
  workspaces: Workspace[];
  sessions: Session[];
  current: Workspace | null;
  // Folders with conversations that the server does not offer now.
  recentFolders: string[];
  onOpen: (workspace: Workspace) => void;
  onOpenFolder?: (create?: boolean) => void;
  onReopenFolder?: (path: string) => void;
  onRemove: (workspace: Workspace) => void;
}) {
  const { t, lang } = useI18n();
  const [query, setQuery] = useState("");
  const needle = query.trim().toLowerCase();
  const stats = useMemo(() => {
    const byPath = new Map<string, { count: number; updated: string }>();
    for (const session of props.sessions) {
      const entry = byPath.get(session.workspace) || { count: 0, updated: "" };
      entry.count++;
      if (session.updatedAt > entry.updated) entry.updated = session.updatedAt;
      byPath.set(session.workspace, entry);
    }
    return byPath;
  }, [props.sessions]);
  const shown = props.workspaces
    .filter((workspace) => !needle || workspace.name.toLowerCase().includes(needle) || workspace.path.toLowerCase().includes(needle))
    // The most recently used first; folders without conversations by name.
    .sort((a, b) => (stats.get(b.path)?.updated || "").localeCompare(stats.get(a.path)?.updated || "") || a.name.localeCompare(b.name));
  const recent = props.onReopenFolder ? props.recentFolders.filter((path) => !needle || path.toLowerCase().includes(needle)) : [];

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      <PageHeader sidebarHidden={props.sidebarHidden} onShowSidebar={props.onShowSidebar} />
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-[860px] px-4 pb-10 pt-2 sm:px-6">
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="mr-auto font-display text-[26px] font-medium tracking-tight text-ink">{t("projects")}</h1>
            {props.onOpenFolder && (
              <>
                <button
                  type="button"
                  onClick={() => props.onOpenFolder?.()}
                  className="flex h-9 items-center gap-1.5 rounded-lg border border-line px-3 text-[14px] text-ink-2 hover:bg-hover hover:text-ink"
                >
                  <FolderOpen size={15} /> {t("openFolder").replace(/…$/, "")}
                </button>
                <button
                  type="button"
                  onClick={() => props.onOpenFolder?.(true)}
                  className="flex h-9 items-center gap-1.5 rounded-lg bg-clay px-3 text-[14px] font-medium text-white hover:bg-clay-hover"
                >
                  <FolderPlus size={15} /> {t("newProjectTitle")}
                </button>
              </>
            )}
          </div>

          <label className="mt-5 flex h-10 items-center gap-2 rounded-lg border border-line bg-surface px-3 text-ink-3 focus-within:border-line-strong">
            <Search size={16} className="shrink-0" />
            <input
              type="search"
              value={query}
              onChange={(event) => setQuery(event.target.value)}
              placeholder={t("searchWorkspaces")}
              className="w-full bg-transparent text-[14px] text-ink outline-none placeholder:text-ink-3"
            />
          </label>

          {!props.workspaces.length && <p className="mt-8 text-center text-[14px] text-ink-3">{t("projectsEmpty")}</p>}
          {props.workspaces.length > 0 && !shown.length && <p className="mt-8 text-center text-[14px] text-ink-3">{t("noMatches")}</p>}

          <ul className="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
            {shown.map((workspace) => {
              const stat = stats.get(workspace.path);
              return (
                <li key={workspace.id} className="group relative min-w-0">
                  <button
                    type="button"
                    onClick={() => props.onOpen(workspace)}
                    title={workspace.path}
                    className={
                      "flex h-full w-full flex-col gap-1 rounded-xl border bg-surface px-4 py-3.5 text-left transition hover:border-line-strong " +
                      (props.current?.id === workspace.id ? "border-line-strong" : "border-line")
                    }
                  >
                    <span className="flex min-w-0 items-center gap-2 pr-6">
                      <FolderGit2 size={16} className="shrink-0 text-ink-3" />
                      <span className="truncate text-[15px] font-medium text-ink">{workspace.name}</span>
                    </span>
                    <span className="truncate text-[12px] text-ink-3">{workspace.path}</span>
                    <span className="mt-1 text-[12.5px] text-ink-2">
                      {t("conversationCount", { n: stat?.count || 0 })}
                      {stat?.updated && <span className="text-ink-3"> · {relativeTime(stat.updated, lang)}</span>}
                    </span>
                  </button>
                  {workspace.added && (
                    <button
                      type="button"
                      title={t("removeFromList")}
                      aria-label={t("removeFromList")}
                      onClick={() => props.onRemove(workspace)}
                      className="absolute right-2 top-2 rounded-md p-1 text-ink-3 opacity-0 hover:bg-hover hover:text-red-500 focus:opacity-100 group-hover:opacity-100 max-md:opacity-100"
                    >
                      <X size={14} />
                    </button>
                  )}
                </li>
              );
            })}
          </ul>

          {recent.length > 0 && (
            <>
              <h2 className="mt-8 text-[13px] font-medium text-ink-3">{t("recentFolders")}</h2>
              <ul className="mt-2 space-y-1">
                {recent.map((path) => (
                  <li key={path}>
                    <button
                      type="button"
                      onClick={() => props.onReopenFolder?.(path)}
                      title={path}
                      className="flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left hover:bg-hover"
                    >
                      <History size={15} className="shrink-0 text-ink-3" />
                      <span className="flex min-w-0 flex-1 flex-col leading-tight">
                        <span className="truncate text-[14px] text-ink-2">{baseName(path)}</span>
                        <span className="truncate text-[11.5px] text-ink-3">{path}</span>
                      </span>
                      <span className="shrink-0 text-[12px] text-ink-3">{t("conversationCount", { n: stats.get(path)?.count || 0 })}</span>
                    </button>
                  </li>
                ))}
              </ul>
            </>
          )}
        </div>
      </div>
    </div>
  );
}

// ProjectDetail is one project: its conversations, the CLAUDE.md files and
// permission rules Claude Code reads in it, and a way to its MCP servers.
export function ProjectDetail(props: Common & {
  api: Api;
  workspace: Workspace;
  sessions: Session[];
  onBack: () => void;
  onNewConversation: () => void;
  onSelect: (id: string) => void;
  onPatchMany: (sessions: Session[], change: { archived?: boolean; pinned?: boolean }) => Promise<void>;
  onDeleteMany: (sessions: Session[]) => Promise<void>;
  providers: ProviderView[];
  defaults: ProjectDefaults;
  onSaveDefaults: (defaults: ProjectDefaults) => Promise<void>;
  onConnectors: () => void;
  registerLeaveGuard: (guard: (() => boolean) | null) => void;
}) {
  const { t } = useI18n();
  const sources = useMemo(() => projectMemorySources(props.api, t), [props.api, t]);
  const sessions = props.sessions
    .filter((session) => session.workspace === props.workspace.path)
    .sort((a, b) => b.updatedAt.localeCompare(a.updatedAt));
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      <PageHeader sidebarHidden={props.sidebarHidden} onShowSidebar={props.onShowSidebar}>
        <button type="button" onClick={props.onBack} className="flex items-center gap-1.5 rounded-md px-1.5 py-1 text-[13px] text-ink-2 hover:bg-hover hover:text-ink">
          <ArrowLeft size={15} /> {t("allProjects")}
        </button>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto w-full max-w-[1080px] px-4 pb-10 pt-2 sm:px-6">
          <div className="flex flex-wrap items-start gap-3">
            <div className="mr-auto min-w-0">
              <h1 className="truncate font-display text-[26px] font-medium tracking-tight text-ink">{props.workspace.name}</h1>
              <p className="truncate text-[12.5px] text-ink-3" title={props.workspace.path}>{props.workspace.path}</p>
            </div>
            <button
              type="button"
              onClick={props.onConnectors}
              className="flex h-9 shrink-0 items-center gap-1.5 rounded-lg border border-line px-3 text-[14px] text-ink-2 hover:bg-hover hover:text-ink"
            >
              <Blocks size={15} /> {t("connectorsShort")}
            </button>
            <button
              type="button"
              onClick={props.onNewConversation}
              className="flex h-9 shrink-0 items-center gap-1.5 rounded-lg bg-clay px-3 text-[14px] font-medium text-white hover:bg-clay-hover"
            >
              <Plus size={15} /> {t("newSession")}
            </button>
          </div>

          <div className="mt-6 grid min-w-0 gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,440px)]">
            <ProjectSessions sessions={sessions} onSelect={props.onSelect} onPatchMany={props.onPatchMany} onDeleteMany={props.onDeleteMany} />
            <div className="flex min-w-0 flex-col gap-4 max-lg:order-first">
              <DefaultsCard key={props.workspace.id} providers={props.providers} defaults={props.defaults} onSave={props.onSaveDefaults} />
              <MemoryEditor key={props.workspace.id} title={t("projectInstructions")} sources={sources} registerLeaveGuard={props.registerLeaveGuard} />
              <PermissionRules key={props.workspace.id} api={props.api} />
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

// ProjectSessions lists a project's conversations, archived ones included,
// and acts on several at once.
function ProjectSessions({ sessions, onSelect, onPatchMany, onDeleteMany }: {
  sessions: Session[];
  onSelect: (id: string) => void;
  onPatchMany: (sessions: Session[], change: { archived?: boolean }) => Promise<void>;
  onDeleteMany: (sessions: Session[]) => Promise<void>;
}) {
  const { t, lang } = useI18n();
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const chosen = sessions.filter((session) => selected.has(session.id));
  const toggle = (id: string) =>
    setSelected((current) => {
      const next = new Set(current);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const run = async (action: () => Promise<void>) => {
    setBusy(true);
    try {
      await action();
      setSelected(new Set());
    } finally {
      setBusy(false);
    }
  };
  const allArchived = chosen.length > 0 && chosen.every((session) => session.archived);
  return (
    <section className="min-w-0">
      <div className="mb-2 flex min-h-8 flex-wrap items-center gap-2">
        {chosen.length ? (
          <>
            <span className="mr-auto text-[13px] text-ink-2">{t("selectedCount", { n: chosen.length })}</span>
            <button type="button" disabled={busy} onClick={() => setSelected(new Set())} className="h-7 rounded-md px-2 text-[12.5px] text-ink-3 hover:bg-hover hover:text-ink">
              {t("cancel")}
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => void run(() => onPatchMany(chosen, { archived: !allArchived }))}
              className="flex h-7 items-center gap-1.5 rounded-md border border-line px-2 text-[12.5px] text-ink-2 hover:bg-hover hover:text-ink disabled:opacity-40"
            >
              {allArchived ? <ArchiveRestore size={13} /> : <Archive size={13} />}
              {allArchived ? t("unarchive") : t("archive")}
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => void run(() => onDeleteMany(chosen))}
              className="flex h-7 items-center gap-1.5 rounded-md border border-line px-2 text-[12.5px] text-red-500 hover:bg-red-500/10 disabled:opacity-40"
            >
              {busy ? <LoaderCircle size={13} className="animate-spin" /> : <Trash2 size={13} />}
              {t("delete")}
            </button>
          </>
        ) : (
          <h2 className="text-[13px] font-medium text-ink-3">{t("conversationCount", { n: sessions.length })}</h2>
        )}
      </div>
      {!sessions.length && <p className="rounded-xl border border-dashed border-line px-4 py-6 text-center text-[13.5px] text-ink-3">{t("noProjectConversations")}</p>}
      <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line empty:hidden">
        {sessions.map((session) => (
          <li key={session.id} className={"group flex items-center bg-surface " + (selected.has(session.id) ? "bg-hover" : "")}>
            <label className="flex shrink-0 cursor-pointer items-center self-stretch pl-3.5 pr-1">
              <input
                type="checkbox"
                checked={selected.has(session.id)}
                onChange={() => toggle(session.id)}
                aria-label={session.title || t("untitled")}
                className={"size-3.5 accent-[var(--clay)] " + (selected.size ? "" : "md:opacity-0 md:group-hover:opacity-100 md:focus:opacity-100")}
              />
            </label>
            <button type="button" onClick={() => onSelect(session.id)} className="flex min-w-0 flex-1 items-center gap-3 px-2.5 py-3 text-left hover:bg-hover">
              {session.pinned ? <Pin size={15} className="shrink-0 text-ink-3" /> : <MessageSquare size={15} className="shrink-0 text-ink-3" />}
              <span className="flex min-w-0 flex-1 flex-col leading-tight">
                <span className={"truncate text-[14px] " + (session.archived ? "text-ink-3" : "text-ink")}>{session.title || t("untitled")}</span>
                <span className="flex min-w-0 items-center gap-1 text-[12px] text-ink-3">
                  {session.worktree && <GitBranch size={11} className="shrink-0" />}
                  <span className="truncate">
                    {[session.archived ? t("archived") : "", session.worktree?.branch, session.provider || "default", session.model].filter(Boolean).join(" · ")}
                  </span>
                </span>
              </span>
              {session.running && <LoaderCircle size={14} className="shrink-0 animate-spin text-clay" aria-label={t("running")} />}
              <span className="shrink-0 text-[12px] text-ink-3">{relativeTime(session.updatedAt, lang)}</span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}

// DefaultsCard sets what new conversations in the project start with.
function DefaultsCard({ providers, defaults, onSave }: { providers: ProviderView[]; defaults: ProjectDefaults; onSave: (defaults: ProjectDefaults) => Promise<void> }) {
  const { t } = useI18n();
  const [draft, setDraft] = useState<ProjectDefaults>(defaults);
  const [state, setState] = useState<"" | "saving" | "saved" | string>("");
  // Defaults that arrive (or change) after the card opened replace the form.
  const loaded = JSON.stringify(defaults);
  useEffect(() => setDraft(JSON.parse(loaded)), [loaded]);
  const dirty = JSON.stringify(clean(draft)) !== JSON.stringify(clean(defaults));
  const provider = providers.find((item) => item.name === draft.provider);
  const save = async () => {
    setState("saving");
    try {
      await onSave(clean(draft));
      setState("saved");
    } catch (error) {
      setState(error instanceof Error ? error.message : String(error));
    }
  };
  const field = "h-8 w-full min-w-0 rounded-lg border border-line bg-canvas px-2 text-[13px] text-ink outline-none focus:border-line-strong";
  return (
    <section className="flex min-w-0 flex-col rounded-xl border border-line bg-surface">
      <div className="flex items-center gap-2 border-b border-line px-4 py-2.5">
        <SlidersHorizontal size={15} className="shrink-0 text-ink-3" />
        <h2 className="mr-auto text-[14px] font-medium text-ink">{t("projectDefaults")}</h2>
      </div>
      <p className="px-4 pt-3 text-[12.5px] leading-relaxed text-ink-3">{t("projectDefaultsHint")}</p>
      <div className="grid grid-cols-1 gap-x-3 gap-y-2.5 px-4 py-3 sm:grid-cols-2">
        <label className="min-w-0 text-[12.5px] text-ink-2">
          {t("provider")}
          <select className={field + " mt-1"} value={draft.provider || ""} onChange={(event) => setDraft({ ...draft, provider: event.target.value, model: "" })}>
            <option value="">{t("serverDefault")}</option>
            <option value="default">default</option>
            {providers.map((item) => (
              <option key={item.name} value={item.name}>{item.name}</option>
            ))}
          </select>
        </label>
        <label className="min-w-0 text-[12.5px] text-ink-2">
          {t("model")}
          <select className={field + " mt-1"} value={draft.model || ""} onChange={(event) => setDraft({ ...draft, model: event.target.value })}>
            <option value="">{t("auto")}</option>
            {(provider?.modelChoices || []).map((choice) => (
              <option key={choice.value} value={choice.value}>{choice.label}</option>
            ))}
            {draft.model && !provider?.modelChoices.some((choice) => choice.value === draft.model) && <option value={draft.model}>{draft.model}</option>}
          </select>
        </label>
        <label className="min-w-0 text-[12.5px] text-ink-2">
          {t("effort")}
          <select className={field + " mt-1"} value={draft.effort || ""} onChange={(event) => setDraft({ ...draft, effort: event.target.value })}>
            <option value="">{t("effortDefault")}</option>
            {EFFORT_LEVELS.map((level) => (
              <option key={level} value={level}>{level}</option>
            ))}
          </select>
        </label>
        <label className="min-w-0 text-[12.5px] text-ink-2">
          {t("permissionMode")}
          <select className={field + " mt-1"} value={draft.permissionMode || ""} onChange={(event) => setDraft({ ...draft, permissionMode: event.target.value })}>
            <option value="">{t("serverDefault")}</option>
            {PERMISSION_MODES.map((mode) => (
              <option key={mode} value={mode}>{t(("mode" + mode[0].toUpperCase() + mode.slice(1)) as "modeDefault")}</option>
            ))}
          </select>
        </label>
      </div>
      <div className="flex items-center gap-2 border-t border-line px-4 py-2.5">
        <span className={"mr-auto truncate text-[12.5px] " + (state && state !== "saving" && state !== "saved" ? "text-red-500" : "text-ink-3")}>
          {dirty ? t("unsaved") : state === "saved" ? t("defaultsSaved") : state === "saving" ? "" : state}
        </span>
        <button
          type="button"
          disabled={!dirty || state === "saving"}
          onClick={() => void save()}
          className="flex h-8 items-center gap-1.5 rounded-lg bg-clay px-3 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
        >
          {state === "saving" && <LoaderCircle size={13} className="animate-spin" />}
          {t("save")}
        </button>
      </div>
    </section>
  );
}

// clean drops empty fields, so "not set" compares and saves the same way.
function clean(defaults: ProjectDefaults): ProjectDefaults {
  return Object.fromEntries(Object.entries(defaults).filter(([, value]) => value)) as ProjectDefaults;
}
