import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { ArrowLeft, FileText, FolderGit2, FolderOpen, FolderPlus, History, LoaderCircle, MessageSquare, PanelLeft, Plus, RotateCcw, Search, X } from "lucide-react";
import { HttpError } from "../api";
import type { Api } from "../api";
import type { Session, Workspace } from "../types";
import { fileSize, relativeTime } from "../format";
import { useI18n } from "../i18n";
import { IconButton } from "./Sidebar";

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

// ProjectDetail is one project: its conversations and the CLAUDE.md files
// Claude Code reads in it.
export function ProjectDetail(props: Common & {
  api: Api;
  workspace: Workspace;
  sessions: Session[];
  onBack: () => void;
  onNewConversation: () => void;
  onSelect: (id: string) => void;
  registerLeaveGuard: (guard: (() => boolean) | null) => void;
}) {
  const { t, lang } = useI18n();
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
              onClick={props.onNewConversation}
              className="flex h-9 shrink-0 items-center gap-1.5 rounded-lg bg-clay px-3 text-[14px] font-medium text-white hover:bg-clay-hover"
            >
              <Plus size={15} /> {t("newSession")}
            </button>
          </div>

          <div className="mt-6 grid min-w-0 gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,440px)]">
            <section className="min-w-0">
              <h2 className="mb-2 text-[13px] font-medium text-ink-3">{t("conversationCount", { n: sessions.length })}</h2>
              {!sessions.length && <p className="rounded-xl border border-dashed border-line px-4 py-6 text-center text-[13.5px] text-ink-3">{t("noProjectConversations")}</p>}
              <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line empty:hidden">
                {sessions.map((session) => (
                  <li key={session.id}>
                    <button type="button" onClick={() => props.onSelect(session.id)} className="flex w-full items-center gap-3 bg-surface px-4 py-3 text-left hover:bg-hover">
                      <MessageSquare size={15} className="shrink-0 text-ink-3" />
                      <span className="flex min-w-0 flex-1 flex-col leading-tight">
                        <span className="truncate text-[14px] text-ink">{session.title || t("untitled")}</span>
                        <span className="truncate text-[12px] text-ink-3">{[session.provider || "default", session.model].filter(Boolean).join(" · ")}</span>
                      </span>
                      {session.running && <LoaderCircle size={14} className="shrink-0 animate-spin text-clay" aria-label={t("running")} />}
                      <span className="shrink-0 text-[12px] text-ink-3">{relativeTime(session.updatedAt, lang)}</span>
                    </button>
                  </li>
                ))}
              </ul>
            </section>
            <MemoryEditor key={props.workspace.id} api={props.api} registerLeaveGuard={props.registerLeaveGuard} />
          </div>
        </div>
      </div>
    </div>
  );
}

type MemoryKind = "shared" | "local";

interface Loaded {
  path: string;
  // Empty for a file that does not exist yet.
  hash: string;
  content: string;
  size: number;
  tooBig?: boolean;
}

async function readOptional(api: Api, path: string): Promise<Loaded | null> {
  try {
    const file = await api.readFile(path);
    return { path: file.path, hash: file.hash || "", content: file.content || "", size: file.size, tooBig: file.tooBig || file.binary };
  } catch (error) {
    if (error instanceof HttpError && error.status === 404) return null;
    throw error;
  }
}

// loadMemory reads the project's CLAUDE.md (at the root, else .claude/) or
// its CLAUDE.local.md. A missing file is edited as a new one.
async function loadMemory(api: Api, kind: MemoryKind): Promise<Loaded> {
  if (kind === "local") return (await readOptional(api, "CLAUDE.local.md")) || { path: "CLAUDE.local.md", hash: "", content: "", size: 0 };
  return (await readOptional(api, "CLAUDE.md")) || (await readOptional(api, ".claude/CLAUDE.md")) || { path: "CLAUDE.md", hash: "", content: "", size: 0 };
}

function MemoryEditor({ api, registerLeaveGuard }: { api: Api; registerLeaveGuard: (guard: (() => boolean) | null) => void }) {
  const { t } = useI18n();
  const [kind, setKind] = useState<MemoryKind>("shared");
  const [file, setFile] = useState<Loaded | null>(null);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [conflict, setConflict] = useState(false);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState("");
  const dirty = !!file && draft !== file.content;

  const load = async (which: MemoryKind) => {
    setFile(null);
    setError("");
    setConflict(false);
    setStatus("");
    try {
      const loaded = await loadMemory(api, which);
      setFile(loaded);
      setDraft(loaded.content);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };
  useEffect(() => {
    void load(kind);
    // The editor remounts for another project, so only the file changes here.
  }, [kind]);

  // Leaving the page (or the browser tab) with unsaved edits asks first.
  const dirtyRef = useRef(dirty);
  dirtyRef.current = dirty;
  useEffect(() => {
    registerLeaveGuard(() => !dirtyRef.current || confirm(t("discardChanges")));
    const onUnload = (event: BeforeUnloadEvent) => {
      if (dirtyRef.current) event.preventDefault();
    };
    window.addEventListener("beforeunload", onUnload);
    return () => {
      registerLeaveGuard(null);
      window.removeEventListener("beforeunload", onUnload);
    };
  }, [registerLeaveGuard, t]);

  const save = async () => {
    if (!file || !dirty || saving) return;
    setSaving(true);
    setError("");
    try {
      const result = await api.writeFile(file.path, draft, file.hash);
      setFile({ ...file, hash: result.hash, content: draft });
      setStatus(t("memoryApplies"));
    } catch (err) {
      if (err instanceof HttpError && err.status === 409) setConflict(true);
      else setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const choose = (next: MemoryKind) => {
    if (next === kind || (dirty && !confirm(t("discardChanges")))) return;
    setKind(next);
  };

  return (
    <section className="flex min-w-0 flex-col rounded-xl border border-line bg-surface max-lg:order-first lg:sticky lg:top-2 lg:self-start">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-4 py-2.5">
        <FileText size={15} className="shrink-0 text-ink-3" />
        <h2 className="mr-auto text-[14px] font-medium text-ink">{t("projectInstructions")}</h2>
        <div className="flex rounded-lg bg-hover p-0.5 text-[12.5px]">
          {(["shared", "local"] as const).map((which) => (
            <button
              key={which}
              type="button"
              onClick={() => choose(which)}
              className={"rounded-md px-2 py-0.5 font-mono " + (kind === which ? "bg-surface text-ink shadow-sm" : "text-ink-3 hover:text-ink")}
            >
              {which === "shared" ? "CLAUDE.md" : "CLAUDE.local.md"}
            </button>
          ))}
        </div>
      </div>
      <div className="px-4 pt-3 text-[12.5px] leading-relaxed text-ink-3">
        {kind === "shared" ? t("memoryShared") : t("memoryLocal")}
        {file && !file.hash && <span className="block pt-1">{t("memoryMissing", { file: file.path })}</span>}
        {file && file.hash && file.path !== "CLAUDE.md" && file.path !== "CLAUDE.local.md" && <span className="block pt-1 font-mono">{file.path}</span>}
      </div>
      <div className="min-w-0 px-4 py-3">
        {!file && !error && (
          <div className="grid h-40 place-items-center text-ink-3">
            <LoaderCircle size={16} className="animate-spin" />
          </div>
        )}
        {file?.tooBig && <p className="py-6 text-center text-[13px] text-ink-3">{t("fileTooBig", { size: fileSize(file.size) })}</p>}
        {file && !file.tooBig && (
          <textarea
            value={draft}
            onChange={(event) => {
              setDraft(event.target.value);
              setStatus("");
            }}
            onKeyDown={(event) => {
              if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
                event.preventDefault();
                void save();
              }
            }}
            spellCheck={false}
            placeholder={t("memoryPlaceholder")}
            className="block h-[min(52vh,520px)] min-h-[220px] w-full resize-y rounded-lg border border-line bg-canvas px-3 py-2.5 font-mono text-[12.5px] leading-relaxed text-ink outline-none placeholder:text-ink-3 focus:border-line-strong"
          />
        )}
      </div>
      {error && <p className="mx-4 mb-3 rounded-lg bg-red-500/10 px-3 py-2 text-[12.5px] text-red-500">{error}</p>}
      {conflict && (
        <div className="mx-4 mb-3 flex flex-wrap items-center gap-2 rounded-lg bg-amber-500/10 px-3 py-2 text-[12.5px] text-ink-2">
          <span className="mr-auto">{t("memoryConflict")}</span>
          <button type="button" onClick={() => void load(kind)} className="flex items-center gap-1 rounded-md px-2 py-0.5 text-ink hover:bg-hover">
            <RotateCcw size={13} /> {t("reload")}
          </button>
        </div>
      )}
      <div className="flex items-center gap-2 border-t border-line px-4 py-2.5">
        <span className="mr-auto truncate text-[12.5px] text-ink-3">{dirty ? t("unsaved") : status}</span>
        {dirty && (
          <button type="button" onClick={() => file && setDraft(file.content)} className="h-8 rounded-lg px-3 text-[13px] text-ink-2 hover:bg-hover hover:text-ink">
            {t("cancel")}
          </button>
        )}
        <button
          type="button"
          disabled={!dirty || saving || conflict}
          onClick={() => void save()}
          className="flex h-8 items-center gap-1.5 rounded-lg bg-clay px-3 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
        >
          {saving && <LoaderCircle size={13} className="animate-spin" />}
          {t("save")}
        </button>
      </div>
    </section>
  );
}
