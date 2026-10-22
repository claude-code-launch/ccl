import { useEffect, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Archive, ArchiveRestore, Check, ChevronRight, ChevronsUpDown, Ellipsis, FolderGit2, FolderOpen, FolderPlus, Folders, GitBranch, History, LoaderCircle, Moon, PanelLeft, Pencil, Pin, PinOff, Plus, Search, Settings, Sun, Trash2, X } from "lucide-react";
import type { ClaudeSession, Health, Session, Workspace } from "../types";
import { basename, groupSessions, relativeTime } from "../format";
import { useI18n } from "../i18n";

interface Props {
  // The active project's conversations; a search looks through allSessions.
  sessions: Session[];
  allSessions: Session[];
  workspaces: Workspace[];
  workspace: Workspace | null;
  onWorkspace: (id: string) => void;
  onRefreshWorkspaces: () => void;
  // Absent when the server was started with --lock-workspaces.
  onOpenFolder?: (create?: boolean) => void;
  // Folders whose conversations are hidden because they are not served.
  recentFolders: string[];
  onReopenFolder?: (path: string) => void;
  onRemoveWorkspace: (workspace: Workspace) => void;
  currentId: string | null;
  // The main column's page; "projects" and "project" light the Projects row.
  page: "chat" | "projects" | "project";
  onProjects: () => void;
  health: Health | null;
  theme: "light" | "dark";
  onSelect: (id: string) => void;
  onCreate: () => void;
  onRename: (session: Session) => void;
  onDelete: (session: Session) => void;
  onPatch: (session: Session, change: { pinned?: boolean; archived?: boolean }) => void;
  onSettings: () => void;
  // The avatar opens cloud sync; sync is its dot (absent: no remote).
  onCloud: () => void;
  sync?: "synced" | "pending";
  onToggleTheme: () => void;
  onCollapse: () => void;
  claudeSessions: ClaudeSession[] | null;
  // A newer ccl release, when there is one.
  update: string;
  onLoadClaudeSessions: () => void;
  onImport: (id: string) => void;
}

// The sidebar follows claude.ai/code: brand and collapse, search, New, the
// recent sessions, and the account row at the bottom.
export function Sidebar(props: Props) {
  const { sessions, currentId, health } = props;
  const { t } = useI18n();
  const [query, setQuery] = useState("");

  const needle = query.trim().toLowerCase();
  // A search covers every project, archived conversations included.
  const matching = needle
    ? props.allSessions.filter((session) => [session.title, session.provider, session.model, session.worktree?.branch].some((value) => (value || "").toLowerCase().includes(needle)))
    : [];
  const live = sessions.filter((session) => !session.archived);
  const archived = sessions.filter((session) => session.archived);
  const groups = groupSessions(live);
  const projectOf = (session: Session) =>
    session.workspace === props.workspace?.path ? "" : props.workspaces.find((item) => item.path === session.workspace)?.name || basename(session.workspace);
  const row = (session: Session) => (
    <SessionRow
      key={session.id}
      session={session}
      project={projectOf(session)}
      active={session.id === currentId}
      onSelect={() => props.onSelect(session.id)}
      onRename={() => props.onRename(session)}
      onDelete={() => props.onDelete(session)}
      onPatch={(change) => props.onPatch(session, change)}
    />
  );

  return (
    <aside className="flex h-full min-h-0 w-full flex-col border-r border-line bg-sidebar">
      <div className="flex h-12 shrink-0 items-center gap-2 px-2.5">
        <IconButton title={t("toggleSidebar")} onClick={props.onCollapse}>
          <PanelLeft size={17} />
        </IconButton>
        <span className="font-display text-[20px] font-medium tracking-tight text-ink">ccl web</span>
      </div>

      {props.workspaces.length > 0 && (
        <WorkspaceSwitcher
          workspaces={props.workspaces}
          current={props.workspace}
          onChoose={props.onWorkspace}
          onOpen={props.onRefreshWorkspaces}
          onOpenFolder={props.onOpenFolder}
          recent={props.onReopenFolder ? props.recentFolders : []}
          onReopen={props.onReopenFolder}
          onRemove={props.onRemoveWorkspace}
        />
      )}

      <div className="px-2">
        <label className="flex h-8 items-center gap-2 rounded-lg border border-line bg-hover px-2.5 text-ink-3 focus-within:border-line-strong">
          <Search size={15} className="shrink-0" />
          <input
            type="search"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={t("searchSessions")}
            className="w-full bg-transparent text-[14px] text-ink outline-none placeholder:text-ink-3"
          />
        </label>
      </div>

      <nav className="mt-2 space-y-0.5 px-2">
        <NavRow icon={<span className="grid size-5 place-items-center rounded-full bg-active"><Plus size={13} /></span>} active={!currentId && props.page === "chat"} onClick={props.onCreate}>
          {t("newSession")}
        </NavRow>
        <NavRow icon={<Folders size={16} />} active={props.page !== "chat"} onClick={props.onProjects}>
          {t("projects")}
        </NavRow>
      </nav>

      <div className="mt-5 flex items-center justify-between px-4 pb-1 text-[13px] text-ink-3">
        <span>{t("recents")}</span>
        <button onClick={props.onCreate} title={t("newSession")} aria-label={t("newSession")} className="rounded-md p-0.5 hover:bg-hover hover:text-ink">
          <Plus size={15} />
        </button>
      </div>
      <ul className="min-h-0 flex-1 space-y-0.5 overflow-y-auto px-2 pb-3">
        {needle ? (
          <>
            {!matching.length && <li className="px-2.5 py-2 text-[13px] text-ink-3">{t("noMatches")}</li>}
            {matching.map(row)}
          </>
        ) : (
          <>
            {!live.length && <li className="px-2.5 py-2 text-[13px] text-ink-3">{t("noSessions")}</li>}
            {groups.map(([label, list], index) => (
              <li key={label}>
                <p className={"px-2.5 pb-0.5 text-[11.5px] text-ink-3 " + (index ? "pt-3" : "")}>{t(label)}</p>
                <ul className="space-y-0.5">{list.map(row)}</ul>
              </li>
            ))}
            {archived.length > 0 && <ArchivedSessions count={archived.length}>{archived.map(row)}</ArchivedSessions>}
          </>
        )}
        <NativeSessions sessions={props.claudeSessions} needle={needle} onLoad={props.onLoadClaudeSessions} onImport={props.onImport} />
      </ul>

      {props.update && (
        <div className="mx-2 mb-2 rounded-lg border border-clay/40 bg-clay/[0.07] px-3 py-2 text-[12.5px] text-ink-2">
          {t("updateAvailable", { version: props.update })}
        </div>
      )}
      <div className="flex h-14 shrink-0 items-center gap-2.5 border-t border-line px-3">
        <button
          type="button"
          onClick={props.onCloud}
          title={t("cloudSync") + (props.sync ? " · " + t(props.sync === "synced" ? "cloudStateUpToDate" : "cloudStateNotSynced") : "")}
          aria-label={t("cloudSync")}
          className="relative grid size-7 shrink-0 place-items-center rounded-full bg-active text-[11px] font-semibold text-ink-2 hover:ring-2 hover:ring-line-strong"
        >
          C
          {props.sync && (
            <span className={"absolute -bottom-0.5 -right-0.5 size-2.5 rounded-full border-2 border-sidebar " + (props.sync === "synced" ? "bg-emerald-500" : "bg-amber-500")} />
          )}
        </button>
        <span className="min-w-0 flex-1 truncate text-[13px] text-ink-2" title={health?.workspace || ""}>
          ccl {health?.version || "dev"}
          <span className="text-ink-3"> · claude {(health?.claudeVersion || "?").replace(/\s*\(Claude Code\)/, "")}</span>
        </span>
        <span className="flex shrink-0 items-center gap-0.5">
          <IconButton title={t("settings")} onClick={props.onSettings}>
            <Settings size={16} />
          </IconButton>
          <IconButton title={props.theme === "dark" ? t("lightTheme") : t("darkTheme")} onClick={props.onToggleTheme}>
            {props.theme === "dark" ? <Sun size={16} /> : <Moon size={16} />}
          </IconButton>
        </span>
      </div>
    </aside>
  );
}

// WorkspaceSwitcher chooses among the directories the server offers, like
// claude.ai/code's repository picker.
function WorkspaceSwitcher({ workspaces, current, onChoose, onOpen, onOpenFolder, recent, onReopen, onRemove }: {
  workspaces: Workspace[];
  current: Workspace | null;
  onChoose: (id: string) => void;
  onOpen: () => void;
  onOpenFolder?: (create?: boolean) => void;
  recent: string[];
  onReopen?: (path: string) => void;
  onRemove: (workspace: Workspace) => void;
}) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState("");
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);
  const needle = filter.trim().toLowerCase();
  const shown = workspaces.filter((workspace) => !needle || workspace.name.toLowerCase().includes(needle) || workspace.path.toLowerCase().includes(needle));
  return (
    <div ref={ref} className="relative px-2 pb-2">
      <button
        onClick={() => {
          if (!open) onOpen();
          setOpen(!open);
          setFilter("");
        }}
        title={current?.path}
        className="flex h-9 w-full items-center gap-2 rounded-lg border border-line px-2.5 text-left text-[14px] text-ink hover:bg-hover"
      >
        <FolderGit2 size={15} className="shrink-0 text-ink-3" />
        <span className="min-w-0 flex-1 truncate">{current?.name || t("workspace")}</span>
        <ChevronsUpDown size={14} className="shrink-0 text-ink-3" />
      </button>
      {open && (
        <div className="absolute left-2 right-2 top-full z-30 mt-1 rounded-[10px] border border-line bg-surface p-1 shadow-lg">
          {workspaces.length + recent.length > 6 && (
            <input
              autoFocus
              value={filter}
              onChange={(event) => setFilter(event.target.value)}
              placeholder={t("searchWorkspaces")}
              className="mb-1 w-full rounded-md border border-line bg-canvas px-2 py-1 text-[13px] text-ink outline-none placeholder:text-ink-3"
            />
          )}
          <div className="max-h-72 overflow-y-auto">
            {shown.map((workspace) => (
              <div key={workspace.id} className="group flex items-center rounded-lg hover:bg-active">
                <button
                  onClick={() => {
                    onChoose(workspace.id);
                    setOpen(false);
                  }}
                  title={workspace.path}
                  className="flex min-w-0 flex-1 items-center gap-2 px-2.5 py-1.5 text-left"
                >
                  <span className="flex min-w-0 flex-1 flex-col leading-tight">
                    <span className="truncate text-[14px] text-ink">{workspace.name}</span>
                    <span className="truncate text-[11px] text-ink-3">{workspace.path}</span>
                  </span>
                  {current?.id === workspace.id && <Check size={14} className="shrink-0 text-clay" />}
                </button>
                {workspace.added && (
                  <button
                    title={t("removeFromList")}
                    aria-label={t("removeFromList")}
                    onClick={() => onRemove(workspace)}
                    className="mr-1 hidden shrink-0 rounded p-1 text-ink-3 hover:bg-hover hover:text-red-500 group-hover:block"
                  >
                    <X size={13} />
                  </button>
                )}
              </div>
            ))}
            {!shown.length && <p className="px-2.5 py-2 text-[13px] text-ink-3">{t("noMatches")}</p>}
            {recent.length > 0 && <p className="px-2.5 pb-1 pt-2 text-[11.5px] text-ink-3">{t("recentFolders")}</p>}
            {recent
              .filter((path) => !needle || path.toLowerCase().includes(needle))
              .map((path) => (
                <button
                  key={path}
                  onClick={() => {
                    setOpen(false);
                    onReopen?.(path);
                  }}
                  title={path}
                  className="flex w-full items-center gap-2 rounded-lg px-2.5 py-1.5 text-left hover:bg-active"
                >
                  <History size={14} className="shrink-0 text-ink-3" />
                  <span className="flex min-w-0 flex-1 flex-col leading-tight">
                    <span className="truncate text-[14px] text-ink-2">{path.split(/[\\/]/).filter(Boolean).pop() || path}</span>
                    <span className="truncate text-[11px] text-ink-3">{path}</span>
                  </span>
                </button>
              ))}
          </div>
          {onOpenFolder && (
            <div className="mt-1 border-t border-line pt-1">
              <button
                onClick={() => {
                  setOpen(false);
                  onOpenFolder(true);
                }}
                className="flex w-full items-center gap-2 rounded-lg px-2.5 py-2 text-left text-[14px] text-ink-2 hover:bg-active hover:text-ink"
              >
                <FolderPlus size={15} className="shrink-0 text-ink-3" />
                {t("newProject")}
              </button>
              <button
                onClick={() => {
                  setOpen(false);
                  onOpenFolder();
                }}
                className="flex w-full items-center gap-2 rounded-lg px-2.5 py-2 text-left text-[14px] text-ink-2 hover:bg-active hover:text-ink"
              >
                <FolderOpen size={15} className="shrink-0 text-ink-3" />
                {t("openFolder")}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function ArchivedSessions({ count, children }: { count: number; children: ReactNode }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  return (
    <li className="pt-3">
      <button onClick={() => setOpen(!open)} className="flex w-full items-center gap-1 px-2 pb-1 text-[13px] text-ink-3 hover:text-ink-2">
        <ChevronRight size={13} className={"transition " + (open ? "rotate-90" : "")} />
        {t("archived")}
        <span className="ml-auto text-xs">{count}</span>
      </button>
      {open && <ul className="space-y-0.5">{children}</ul>}
    </li>
  );
}

// Claude Code's own sessions for this workspace, not yet opened here.
function NativeSessions({ sessions, needle, onLoad, onImport }: {
  sessions: ClaudeSession[] | null;
  needle: string;
  onLoad: () => void;
  onImport: (id: string) => void;
}) {
  const { t, lang } = useI18n();
  const [open, setOpen] = useState(false);
  const [importing, setImporting] = useState("");
  const pending = (sessions || []).filter((session) => !session.webSession && (!needle || session.title.toLowerCase().includes(needle)));
  return (
    <li className="pt-4">
      <button
        onClick={() => {
          if (!open) onLoad();
          setOpen(!open);
        }}
        className="flex w-full items-center gap-1 px-2 pb-1 text-[13px] text-ink-3 hover:text-ink-2"
      >
        <ChevronRight size={13} className={"transition " + (open ? "rotate-90" : "")} />
        {t("claudeSessions")}
        {sessions && <span className="ml-auto text-xs">{pending.length}</span>}
      </button>
      {open && (
        <ul className="space-y-0.5">
          {sessions === null && <li className="px-2.5 py-1.5 text-[13px] text-ink-3"><LoaderCircle size={13} className="inline animate-spin" /></li>}
          {sessions && !pending.length && <li className="px-2.5 py-1.5 text-[13px] text-ink-3">{t("noClaudeSessions")}</li>}
          {pending.map((session) => (
            <li key={session.id}>
              <button
                onClick={async () => {
                  setImporting(session.id);
                  try {
                    await onImport(session.id);
                  } finally {
                    setImporting("");
                  }
                }}
                title={`${session.title}\n${t("importHint")}`}
                className="flex h-8 w-full items-center gap-2 rounded-lg pl-2.5 pr-2 text-left text-[14px] text-ink-2 hover:bg-hover hover:text-ink"
              >
                {importing === session.id ? <LoaderCircle size={14} className="shrink-0 animate-spin" /> : <History size={14} className="shrink-0 text-ink-3" />}
                <span className="min-w-0 flex-1 truncate">{session.title || session.id}</span>
                <span className="shrink-0 text-[11px] text-ink-3">{relativeTime(session.updated, lang)}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}

function NavRow({ icon, active, onClick, children }: { icon: ReactNode; active?: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      onClick={onClick}
      className={"flex h-8 w-full items-center gap-2.5 rounded-lg px-2 text-left text-[14px] text-ink " + (active ? "bg-active" : "hover:bg-hover")}
    >
      <span className="grid w-5 shrink-0 place-items-center text-ink-2">{icon}</span>
      {children}
    </button>
  );
}

function SessionRow({ session, project, active, onSelect, onRename, onDelete, onPatch }: {
  session: Session;
  // The project's name when it is not the active one (search results).
  project: string;
  active: boolean;
  onSelect: () => void;
  onRename: () => void;
  onDelete: () => void;
  onPatch: (change: { pinned?: boolean; archived?: boolean }) => void;
}) {
  const { t } = useI18n();
  const [menu, setMenu] = useState(false);
  const ref = useRef<HTMLLIElement>(null);
  useEffect(() => {
    if (!menu) return;
    const close = (event: MouseEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) setMenu(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [menu]);

  return (
    <li ref={ref} className="relative">
      <div
        onClick={onSelect}
        onDoubleClick={onRename}
        title={`${session.title || t("untitled")}\n${session.provider}${session.model ? " · " + session.model : ""}`}
        className={
          "group/row flex h-8 cursor-pointer items-center gap-2 rounded-lg pl-2.5 pr-1 text-[14px] " +
          (active ? "bg-active text-ink" : "text-ink-2 hover:bg-hover hover:text-ink")
        }
      >
        <span className="min-w-0 flex-1 truncate">{session.title || t("untitled")}</span>
        {project && <span className="max-w-[40%] shrink-0 truncate text-[11.5px] text-ink-3">{project}</span>}
        {session.worktree && <GitBranch size={13} className="shrink-0 text-ink-3" aria-label={session.worktree.branch} />}
        {session.running && <LoaderCircle size={14} className="shrink-0 animate-spin text-clay" aria-label={t("running")} />}
        <button
          onClick={(event) => {
            event.stopPropagation();
            setMenu(!menu);
          }}
          title={t("rename")}
          aria-label="…"
          className={"grid size-6 shrink-0 place-items-center rounded-md text-ink-3 hover:bg-active hover:text-ink " + (menu || active ? "" : "invisible group-hover/row:visible")}
        >
          <Ellipsis size={15} />
        </button>
      </div>
      {menu && (
        <div className="absolute right-0 top-full z-30 mt-1 min-w-[150px] rounded-[10px] border border-line bg-surface p-1 shadow-lg">
          <MenuButton icon={session.pinned ? <PinOff size={14} /> : <Pin size={14} />} onClick={() => { setMenu(false); onPatch({ pinned: !session.pinned }); }}>
            {session.pinned ? t("unpin") : t("pin")}
          </MenuButton>
          <MenuButton icon={session.archived ? <ArchiveRestore size={14} /> : <Archive size={14} />} onClick={() => { setMenu(false); onPatch({ archived: !session.archived }); }}>
            {session.archived ? t("unarchive") : t("archive")}
          </MenuButton>
          <MenuButton icon={<Pencil size={14} />} onClick={() => { setMenu(false); onRename(); }}>{t("rename")}</MenuButton>
          <MenuButton icon={<Trash2 size={14} />} danger onClick={() => { setMenu(false); onDelete(); }}>{t("delete")}</MenuButton>
        </div>
      )}
    </li>
  );
}

export function MenuButton({ icon, danger, onClick, children }: { icon: ReactNode; danger?: boolean; onClick: () => void; children: ReactNode }) {
  return (
    <button
      onClick={onClick}
      className={"flex w-full items-center gap-2.5 rounded-lg px-2.5 py-1.5 text-left text-[14px] hover:bg-active " + (danger ? "text-red-500" : "text-ink")}
    >
      {icon}
      {children}
    </button>
  );
}

export function IconButton({ children, title, onClick, active }: { children: ReactNode; title: string; onClick: () => void; active?: boolean }) {
  return (
    <button
      type="button"
      title={title}
      aria-label={title}
      onClick={onClick}
      className={"grid size-8 shrink-0 place-items-center rounded-lg text-ink-2 transition hover:bg-hover hover:text-ink " + (active ? "bg-active text-ink" : "")}
    >
      {children}
    </button>
  );
}
