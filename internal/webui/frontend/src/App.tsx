import { Suspense, lazy, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Folders, Moon, PanelRight, PanelRightClose, Paperclip, Plus, Settings, SquareTerminal, Sun, X } from "lucide-react";
import { Api, HttpError, Unauthorized, storeToken, storedToken } from "./api";
import type { Attachment, Capabilities, ClaudeSession, ConfigView, GitStatus, Health, PermissionDecision, ProjectDefaults, ProviderView, Schedule, Session, StoredEvent, Workspace } from "./types";
import { agentsOf, agentThread, buildThread, latestCapabilities, pendingPermissions, toolActive } from "./thread";
import { AgentBanner, AgentsPanel } from "./components/AgentsPanel";
import { diffSection, download, safeFileName, transcriptHTML, transcriptMarkdown } from "./export";
import { CommandPalette } from "./components/CommandPalette";
import { useConversation } from "./useConversation";
import { settleQueue, steerFirst } from "./queue";
import type { Queued } from "./queue";
import { I18n, resolveLang, translator } from "./i18n";
import { Sidebar, SidebarIcon } from "./components/Sidebar";
import { TabBar } from "./components/TabBar";
import type { PanelTab, TabItem } from "./components/TabBar";
import { RightPanel } from "./components/RightPanel";
import { FilesPanel } from "./components/FilesPanel";
import { GitPanel } from "./components/GitPanel";
import { Thread } from "./components/Thread";
import { Composer } from "./components/Composer";
import { SettingsDialog } from "./components/SettingsDialog";
import { FolderPicker } from "./components/FolderPicker";
import { ShareDialog } from "./components/Share";
import { ProjectDetail, ProjectsList } from "./components/ProjectsPage";
import { Gate } from "./components/Gate";

// The terminal (xterm.js) loads only when a terminal tab opens.
const TerminalPanel = lazy(() => import("./components/TerminalPanel"));

type Theme = "light" | "dark";

// narrowScreen matches Tailwind's below-lg layout, where panels are overlays.
function narrowScreen(): boolean {
  return typeof window !== "undefined" && window.matchMedia("(max-width: 1023.98px)").matches;
}

function readStorage(key: string): string {
  try {
    return localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}

function writeStorage(key: string, value: string) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // A per-browser convenience only.
  }
}

// The theme follows the system until the user picks one with the switch.
function useTheme(): [Theme, () => void] {
  const media = useMemo(() => window.matchMedia("(prefers-color-scheme: dark)"), []);
  const [choice, setChoice] = useState(() => readStorage("ccl-web-theme"));
  const [system, setSystem] = useState<Theme>(media.matches ? "dark" : "light");
  useEffect(() => {
    const onChange = (event: MediaQueryListEvent) => setSystem(event.matches ? "dark" : "light");
    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, [media]);
  const theme: Theme = choice === "light" || choice === "dark" ? choice : system;
  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
  }, [theme]);
  const toggle = () => {
    const next = theme === "dark" ? "light" : "dark";
    setChoice(next);
    writeStorage("ccl-web-theme", next);
  };
  return [theme, toggle];
}

// readList reads a JSON array kept in localStorage; anything else is empty.
function readList<T>(key: string): T[] {
  try {
    const value = JSON.parse(readStorage(key) || "[]");
    return Array.isArray(value) ? value : [];
  } catch {
    return [];
  }
}

// A terminal tab. id is the server's shell, null until it starts; the folder
// is the one that was current when it opened.
interface TermTab {
  key: string;
  id: string | null;
  workspaceId: string;
  worktree: string;
  n: number;
}

// A sub-agent tab: the key of its Agent call, and the title it opened with
// (the call is known only while its conversation is loaded).
interface AgentTab {
  key: string;
  title: string;
  type: string;
}

// agentTabKey names a sub-agent tab; session ids have no colon.
const agentTabKey = (sessionId: string, toolKey: string) => `a:${sessionId}:${toolKey}`;
function parseAgentTab(key: string): [string, string] {
  const rest = key.slice(2);
  const at = rest.indexOf(":");
  return [rest.slice(0, at), rest.slice(at + 1)];
}

const narrow = () => window.matchMedia("(max-width: 767px)").matches;
const NO_CAPABILITIES: Capabilities = { slashCommands: [], mcpServers: [] };

// contextOf is the size of the newest model call against the model's window:
// a provider's Balanced preset caps every slot to its own window (presetWindow);
// otherwise a model id marked [1m] runs with the 1M window.
function contextOf(events: StoredEvent[], session: Session | null, presetWindow?: number): { tokens: number; window: number } | undefined {
  let tokens = 0;
  let model = session?.model || "";
  for (let i = events.length - 1; i >= 0; i--) {
    const event = events[i];
    if (!tokens && event.type === "context") tokens = Number((event.payload as { tokens?: number }).tokens) || 0;
    if (event.type === "init") {
      model = String((event.payload as { model?: string }).model || model);
      break;
    }
  }
  if (!tokens) return undefined;
  if (presetWindow) return { tokens, window: presetWindow };
  return { tokens, window: /\[1m\]|-1m\b/i.test(model) ? 1_000_000 : 200_000 };
}

export function App() {
  const [token, setToken] = useState(storedToken);
  const api = useMemo(() => new Api(token), [token]);
  const [phase, setPhase] = useState<"loading" | "gate" | "ready">("loading");
  const [gateError, setGateError] = useState("");
  const [health, setHealth] = useState<Health | null>(null);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [currentId, setCurrentId] = useState<string | null>(null);
  const [config, setConfig] = useState<ConfigView | null>(null);
  const [configError, setConfigError] = useState("");
  const [serverCapabilities, setServerCapabilities] = useState<Capabilities>(NO_CAPABILITIES);
  // The settings dialog and the section it opens on.
  const [settingsOpen, setSettingsOpenState] = useState<false | "general" | "providers" | "login" | "connectors" | "instructions" | "cloud">(false);
  const setSettingsOpen = (open: boolean | "general" | "providers" | "login" | "connectors" | "instructions" | "cloud") => setSettingsOpenState(open === true ? "general" : open);
  const [mobileSidebar, setMobileSidebar] = useState(false);
  const [collapsed, setCollapsed] = useState(() => readStorage("ccl-web-sidebar") === "collapsed");
  // The side panel's tab; null hides it. Wide when a file or diff is open.
  // On a phone the panel covers the thread, so it starts closed there.
  const [panel, setPanel] = useState<PanelTab | null>(() => {
    if (narrowScreen()) return null;
    const saved = readStorage("ccl-web-panel");
    return saved === "closed" ? null : saved === "files" || saved === "git" || saved === "agents" ? saved : "overview";
  });
  const [panelWide, setPanelWide] = useState(false);
  const [gitFocus, setGitFocus] = useState("");
  // Bumped when a turn ends, so open panels reload what Claude may have changed.
  const [refreshKey, setRefreshKey] = useState(0);
  const [dragging, setDragging] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [shareOpen, setShareOpen] = useState(false);
  const [claudeSessions, setClaudeSessions] = useState<ClaudeSession[] | null>(null);
  const [git, setGit] = useState<GitStatus | null>(null);
  const [queues, setQueues] = useState<Record<string, Queued[]>>({});
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [update, setUpdate] = useState("");
  // The avatar's dot: the primary cloud remote is in sync, or not.
  const [sync, setSync] = useState<"synced" | "pending" | undefined>(undefined);
  const loadSync = useCallback(async () => {
    try {
      const { remotes } = await api.cloud();
      const primary = remotes.find((remote) => remote.primary && remote.signedIn);
      setSync(!primary || primary.error ? undefined : primary.state === "up to date" ? "synced" : "pending");
    } catch {
      setSync(undefined);
    }
  }, [api]);
  const [notice, setNotice] = useState("");
  const [theme, toggleTheme] = useTheme();

  // Workspaces: the conversation's own, else the one chosen in the sidebar.
  // File, git, terminal, MCP and session-import requests go to it.
  const [workspaceList, setWorkspaceList] = useState<Workspace[]>([]);
  // canAddWorkspace is false under --lock-workspaces; the picker adds folders.
  const [canAddWorkspace, setCanAddWorkspace] = useState(false);
  // The folder picker: closed, "open" or "create" (a new project folder).
  const [folderPicker, setFolderPicker] = useState<false | "open" | "create">(false);
  const [chosenWorkspace, setChosenWorkspace] = useState(() => readStorage("ccl-web-workspace"));
  // What the main column shows: the conversation, the projects page, or the
  // active project's page.
  const [page, setPage] = useState<"chat" | "projects" | "project">("chat");
  // A page with unsaved edits (a CLAUDE.md) asks before it is left.
  const leaveGuard = useRef<(() => boolean) | null>(null);
  const registerLeaveGuard = useCallback((guard: (() => boolean) | null) => {
    leaveGuard.current = guard;
  }, []);
  const canLeave = () => !leaveGuard.current || leaveGuard.current();
  const currentSession = sessions.find((session) => session.id === currentId) || null;
  const activeWorkspace =
    (currentSession && workspaceList.find((workspace) => workspace.path === currentSession.workspace)) ||
    workspaceList.find((workspace) => workspace.id === chosenWorkspace) ||
    workspaceList[0] ||
    null;
  // A conversation on a branch of its own sends its file, git and terminal
  // requests to its worktree.
  const worktreeName = currentSession?.worktree?.name || "";
  const wsApi = useMemo(() => (activeWorkspace?.id ? api.forWorkspace(activeWorkspace.id, worktreeName) : api), [api, activeWorkspace?.id, worktreeName]);
  // Panels and terminals belong to one directory: a workspace or a worktree.
  const scopeKey = `${activeWorkspace?.id || ""}:${worktreeName}`;
  // The next new conversation runs in a worktree of its own.
  const [newWorktree, setNewWorktree] = useState(false);
  // Each project's defaults, and the route the next new conversation starts
  // with: the active project's defaults until the user changes it.
  const [projectDefaults, setProjectDefaults] = useState<Record<string, ProjectDefaults>>({});
  const activeDefaults = projectDefaults[activeWorkspace?.id || ""];
  const [draftRoute, setDraftRoute] = useState<ProjectDefaults>({});
  const defaultsKey = JSON.stringify(activeDefaults || {});
  useEffect(() => {
    setDraftRoute(JSON.parse(defaultsKey));
  }, [activeWorkspace?.id, defaultsKey]);
  // The tab bar: open conversations (the current one is currentId), the
  // new-conversation draft, and terminals. Only the active conversation streams
  // its events; the others show the running state of the session list.
  const [openIds, setOpenIds] = useState<string[]>(() => readList<string>("ccl-web-tabs").filter((id) => typeof id === "string"));
  const [termTabs, setTermTabs] = useState<TermTab[]>(() => readList<TermTab>("ccl-web-terminals").filter((tab) => tab && typeof tab.key === "string"));
  // The terminal tab in front; null shows the conversation.
  const [activeTerm, setActiveTerm] = useState<string | null>(null);
  // Sub-agent tabs, by conversation: they sit in a group after its tab.
  const [agentTabs, setAgentTabs] = useState<Record<string, AgentTab[]>>(() => {
    try {
      const value = JSON.parse(readStorage("ccl-web-agent-tabs") || "{}");
      return value && typeof value === "object" && !Array.isArray(value) ? value : {};
    } catch {
      return {};
    }
  });
  useEffect(() => writeStorage("ccl-web-agent-tabs", JSON.stringify(agentTabs)), [agentTabs]);
  // The draft tab was closed while terminals stay open.
  const [draftOpen, setDraftOpen] = useState(true);
  useEffect(() => writeStorage("ccl-web-tabs", JSON.stringify(openIds)), [openIds]);
  useEffect(() => writeStorage("ccl-web-terminals", JSON.stringify(termTabs)), [termTabs]);

  const lang = resolveLang(config?.lang);
  const i18n = useMemo(() => ({ lang, t: translator(lang) }), [lang]);
  const { t } = i18n;
  useEffect(() => {
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
  }, [lang]);

  const signOut = useCallback((message: string) => {
    storeToken("");
    setToken("");
    setGateError(message);
    setPhase("gate");
  }, []);

  const fail = useCallback(
    (error: unknown) => {
      if (error instanceof Unauthorized) signOut("Token rejected");
      else setNotice(error instanceof Error ? error.message : String(error));
    },
    [signOut],
  );

  const loadSessions = useCallback(async () => {
    try {
      setSessions(await api.sessions());
    } catch (error) {
      fail(error);
    }
  }, [api, fail]);

  const loadConfig = useCallback(async () => {
    try {
      setConfig(await api.config());
      setConfigError("");
    } catch (error) {
      if (error instanceof Unauthorized) fail(error);
      else setConfigError(error instanceof Error ? error.message : String(error));
    }
  }, [api, fail]);

  const loadGit = useCallback(async () => {
    try {
      setGit(await wsApi.gitStatus());
    } catch {
      setGit(null);
    }
  }, [wsApi]);

  const loadClaudeSessions = useCallback(async () => {
    try {
      setClaudeSessions((await wsApi.claudeSessions()).sessions);
    } catch (error) {
      setClaudeSessions([]);
      fail(error);
    }
  }, [wsApi, fail]);

  const loadWorkspaces = useCallback(async () => {
    try {
      const result = await api.workspaces();
      setWorkspaceList(result.workspaces);
      setCanAddWorkspace(result.canAdd);
      setProjectDefaults(result.defaults || {});
    } catch {
      // The list from /health stays.
    }
  }, [api]);

  const loadCapabilities = useCallback(async () => {
    try {
      setServerCapabilities(await api.capabilities());
    } catch {
      // Menus fall back to the conversation's own init event.
    }
  }, [api]);

  // Boot: a loopback ccl serve asks for no token at all.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const info = await api.health();
        if (cancelled) return;
        setHealth(info);
        const workspaces = info.workspaces?.length ? info.workspaces : [{ id: "", name: info.workspace.split(/[\\/]/).pop() || info.workspace, path: info.workspace }];
        setWorkspaceList(workspaces);
        if (info.authRequired && !api.token) {
          setPhase("gate");
          return;
        }
        const list = await api.sessions();
        if (cancelled) return;
        setSessions(list);
        // Open the latest conversation of the workspace chosen last time.
        // A conversation of a folder no longer served is not opened.
        const chosen = workspaces.find((workspace) => workspace.id === readStorage("ccl-web-workspace")) || workspaces[0];
        const first = list.find((session) => session.workspace === chosen?.path);
        setCurrentId((current) => current || first?.id || null);
        // The workspace effect loads git status once the page is ready.
        setPhase("ready");
        loadConfig();
        loadCapabilities();
        loadWorkspaces();
        api.updateInfo().then((info) => info.newer && setUpdate(info.latest)).catch(() => {});
        void loadSync();
      } catch (error) {
        if (cancelled) return;
        if (error instanceof Unauthorized) signOut(api.token ? "Token rejected" : "");
        else signOut("Could not connect: " + (error instanceof Error ? error.message : String(error)));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api, loadConfig, loadCapabilities, loadWorkspaces, loadSync, signOut]);

  // Turn boundaries change titles, counts, usage and what Claude Code offers;
  // refresh shortly after rather than once per event.
  const refreshTimer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);
  const scheduleRefresh = useCallback(() => {
    if (refreshTimer.current) return;
    refreshTimer.current = setTimeout(() => {
      refreshTimer.current = undefined;
      setRefreshKey((key) => key + 1);
      loadSessions();
      loadCapabilities();
      loadGit();
    }, 600);
  }, [loadSessions, loadCapabilities, loadGit]);

  const mergeSession = useCallback((session: Session) => {
    setSessions((list) => {
      const found = list.some((item) => item.id === session.id);
      return found ? list.map((item) => (item.id === session.id ? { ...item, ...session } : item)) : [session, ...list];
    });
  }, []);

  const conversation = useConversation(api, phase === "ready" ? currentId : null, scheduleRefresh, mergeSession);
  const items = useMemo(() => buildThread(conversation.events), [conversation.events]);
  const agents = useMemo(() => agentsOf(items), [items]);
  const agentsRunning = agents.filter(({ tool }) => toolActive(tool)).length;
  // The sub-agent the main view shows instead of the conversation, like
  // choosing an agent in Claude Code's agent list.
  const [agentView, setAgentView] = useState<string | null>(null);
  // A sub-agent tab of another conversation opens it, then the sub-agent.
  const nextAgent = useRef<string | null>(null);
  useEffect(() => {
    setAgentView(nextAgent.current);
    nextAgent.current = null;
  }, [currentId]);
  const viewed = agentView ? agents.find(({ tool }) => tool.key === agentView)?.tool : undefined;
  const shownItems = useMemo(() => (viewed ? agentThread(viewed) : items), [viewed, items]);
  const capabilities = useMemo(() => {
    const own = latestCapabilities(conversation.events);
    return own ? { ...own, defaultPermissionMode: serverCapabilities.defaultPermissionMode } : serverCapabilities;
  }, [conversation.events, serverCapabilities]);
  const pending = useMemo(() => pendingPermissions(items), [items]);

  const respondPermission = useCallback(
    async (decision: PermissionDecision) => {
      if (!currentId) return;
      try {
        await api.respondPermission(currentId, decision);
        if (decision.mode) scheduleRefresh();
      } catch (error) {
        fail(error);
      }
    },
    [api, currentId, fail, scheduleRefresh],
  );

  // A question while the page is in the background deserves a notification.
  const notified = useRef(new Set<string>());
  useEffect(() => {
    for (const request of pending) {
      if (notified.current.has(request.requestId)) continue;
      notified.current.add(request.requestId);
      if (document.hidden && typeof Notification !== "undefined" && Notification.permission === "granted") {
        new Notification(t("permNotify"), { body: request.description || request.summary || request.toolName, tag: request.requestId });
      }
    }
  }, [pending, t]);

  const current = sessions.find((session) => session.id === currentId) || null;
  const providers: ProviderView[] = useMemo(
    () => Object.values(config?.providers || {}).sort((a, b) => a.name.localeCompare(b.name)),
    [config],
  );
  const workspace = activeWorkspace?.path || health?.workspace || "";

  // Another workspace has other files, history and native sessions.
  useEffect(() => {
    if (phase !== "ready") return;
    setClaudeSessions(null);
    setRefreshKey((key) => key + 1);
    void loadGit();
  }, [scopeKey, phase, loadGit]);

  const chooseWorkspace = (id: string) => {
    if (page === "project" && id !== activeWorkspace?.id && !canLeave()) return;
    setChosenWorkspace(id);
    writeStorage("ccl-web-workspace", id);
    // A conversation of another workspace is not where the user is now.
    if (currentSession && workspaceList.find((workspace) => workspace.id === id)?.path !== currentSession.workspace) setCurrentId(null);
  };
  // Opening a folder adds it to the server's list and starts a new
  // conversation there.
  const openFolder = canAddWorkspace ? (create?: boolean) => setFolderPicker(create ? "create" : "open") : undefined;
  // Folders with conversations that the server does not offer now; opening
  // one again brings its conversations back.
  const recentFolders = [...new Set(sessions.map((session) => session.workspace))].filter(
    (path) => path && !workspaceList.some((workspace) => workspace.path === path),
  );
  const pickFolder = async (path: string) => {
    const added = await api.addWorkspace(path);
    await loadWorkspaces();
    setCurrentId(null);
    setChosenWorkspace(added.id);
    writeStorage("ccl-web-workspace", added.id);
    setFolderPicker(false);
    setMobileSidebar(false);
    // From the projects page a folder opens as a project; elsewhere it starts
    // a conversation there.
    if (page !== "chat") setPage("project");
  };
  const showProjects = () => {
    if (page === "project" && !canLeave()) return;
    setPage("projects");
    setMobileSidebar(false);
  };
  const openProject = (target: Workspace) => {
    chooseWorkspace(target.id);
    setPage("project");
  };
  const removeWorkspace = async (target: Workspace) => {
    if (!confirm(t("confirmRemoveWorkspace", { name: target.name }))) return;
    try {
      await api.removeWorkspace(target.id);
      await loadWorkspaces();
      if (activeWorkspace?.id === target.id) {
        setCurrentId(null);
        const next = workspaceList.find((workspace) => workspace.id !== target.id);
        if (next) chooseWorkspace(next.id);
      }
    } catch (error) {
      fail(error);
    }
  };
  const route = current ? [current.provider || "default", current.model, current.effort].filter(Boolean).join(" · ") : "";

  const select = (id: string) => {
    if (page === "project" && !canLeave()) return;
    setPage("chat");
    setCurrentId(id);
    setActiveTerm(null);
    setMobileSidebar(false);
    // Opening a conversation also moves the sidebar to its workspace.
    const session = sessions.find((item) => item.id === id);
    const owner = session && workspaceList.find((workspace) => workspace.path === session.workspace);
    if (owner) {
      setChosenWorkspace(owner.id);
      writeStorage("ccl-web-workspace", owner.id);
    }
  };

  // A session the composer created for its first upload keeps the composer
  // (and the files in it): its key stays "new".
  const adopted = useRef<string | null>(null);

  const create = async (adopt = false): Promise<Session | null> => {
    try {
      const session = await api.createSession(activeWorkspace?.id || "", newWorktree, draftRoute);
      setNewWorktree(false);
      if (adopt) adopted.current = session.id;
      mergeSession(session);
      select(session.id);
      return session;
    } catch (error) {
      fail(error);
      return null;
    }
  };

  // Starting a new session is lazy: the first message or file creates it.
  const startNew = () => {
    if (page === "project" && !canLeave()) return;
    setPage("chat");
    setCurrentId(null);
    setActiveTerm(null);
    setDraftOpen(true);
    setDraftRoute(JSON.parse(defaultsKey));
    setMobileSidebar(false);
  };

  const ensureSession = async (): Promise<string | null> => {
    if (currentId) return currentId;
    const session = await create(true);
    return session?.id || null;
  };

  const upload = async (files: File[]): Promise<Attachment[]> => {
    const id = await ensureSession();
    if (!id) throw new Error("no session");
    return api.upload(id, files);
  };

  const send = async (text: string, attachments: string[]): Promise<boolean> => {
    const id = await ensureSession();
    if (!id) return false;
    // Notifications need the user's permission, asked on a click.
    if (typeof Notification !== "undefined" && Notification.permission === "default") void Notification.requestPermission();
    if (conversation.running && id === currentId) {
      const item = { key: `${Date.now()}-${Math.random().toString(36).slice(2)}`, text, attachments };
      setQueues((all) => ({ ...all, [id]: [...(all[id] || []), item] }));
      return true;
    }
    try {
      await api.send(id, text, attachments);
      conversation.setRunning(true);
      scheduleRefresh();
      return true;
    } catch (error) {
      fail(error);
      return false;
    }
  };

  // A queued message goes out once the turn before it ended.
  const queue = (currentId && queues[currentId]) || [];
  const flushing = useRef(false);
  // Keys of steered messages whose request is still out.
  const steering = useRef(new Set<string>());
  useEffect(() => {
    if (!currentId || conversation.loading || flushing.current) return;
    const settled = settleQueue(queue, conversation.events, conversation.running, steering.current);
    if (settled) {
      setQueues((all) => ({ ...all, [currentId]: settled }));
      return;
    }
    if (conversation.running || !queue.length || steering.current.size) return;
    const [next, ...rest] = queue;
    flushing.current = true;
    setQueues((all) => ({ ...all, [currentId]: rest }));
    api
      .send(currentId, next.text, next.attachments)
      .then(() => {
        conversation.setRunning(true);
        scheduleRefresh();
      })
      .catch(fail)
      .finally(() => {
        flushing.current = false;
      });
  }, [currentId, conversation, queue, api, fail, scheduleRefresh]);

  // Steering sends a queued message into the running turn; it stays in the
  // queue, marked sent, until Claude Code has read it.
  const steer = async (key: string) => {
    const id = currentId;
    const item = id ? (queues[id] || []).find((entry) => entry.key === key) : undefined;
    if (!id || !item || item.steered !== undefined) return;
    const since = conversation.events.length ? conversation.events[conversation.events.length - 1].seq : 0;
    const update = (change: (queue: Queued[]) => Queued[]) => setQueues((all) => ({ ...all, [id]: change(all[id] || []) }));
    update((queue) => steerFirst(queue, key, since));
    steering.current.add(key);
    try {
      const response = await api.send(id, item.text, item.attachments, true);
      // The turn had ended: the message started the next one.
      if (!response.steered) update((queue) => queue.filter((entry) => entry.key !== key));
      scheduleRefresh();
    } catch (error) {
      update((queue) => queue.map((entry) => (entry.key === key ? { ...entry, steered: undefined } : entry)));
      fail(error);
    } finally {
      steering.current.delete(key);
    }
  };

  // Tell the user when a turn ends while the page is in the background.
  const wasRunning = useRef(false);
  useEffect(() => {
    const title = current?.title || "ccl";
    document.title = conversation.running ? `● ${title}` : title;
    if (wasRunning.current && !conversation.running && document.hidden && typeof Notification !== "undefined" && Notification.permission === "granted") {
      const last = [...items].reverse().find((item) => item.kind === "turn-end");
      const state = last && last.kind === "turn-end" ? (last.result.cancelled ? "turnStopped" : last.result.isError ? "turnFailed" : "turnDone") : "turnDone";
      new Notification(t(state), { body: title, tag: currentId || undefined });
    }
    wasRunning.current = conversation.running;
  }, [conversation.running, current?.title, currentId, items, t]);

  // ⌘K / Ctrl+K opens the command palette.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setPaletteOpen((open) => !open);
        if (claudeSessions === null) loadClaudeSessions();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [claudeSessions, loadClaudeSessions]);

  const importSession = async (id: string) => {
    try {
      const session = await wsApi.importClaudeSession(id);
      mergeSession(session);
      select(session.id);
      loadClaudeSessions();
    } catch (error) {
      fail(error);
    }
  };

  const exportTranscript = async (format: "md" | "html") => {
    const title = current?.title || "conversation";
    let markdown = transcriptMarkdown(title, items, t);
    // A worktree conversation's export carries what it changed.
    if (current?.worktree) {
      try {
        markdown += diffSection(await api.worktreeDiff(current.id), current.worktree.branch, t);
      } catch {
        // The worktree may be gone; the conversation still exports.
      }
    }
    if (format === "md") download(safeFileName(title) + ".md", markdown, "text/markdown");
    else download(safeFileName(title) + ".html", await transcriptHTML(title, markdown), "text/html");
  };

  // Scheduled messages of the open conversation; a turn boundary may have
  // sent one.
  const loadSchedules = useCallback(async () => {
    if (!currentId) {
      setSchedules([]);
      return;
    }
    try {
      setSchedules((await api.schedules(currentId)).schedules);
    } catch {
      setSchedules([]);
    }
  }, [api, currentId]);
  useEffect(() => {
    void loadSchedules();
  }, [loadSchedules, refreshKey]);

  const scheduleMessage = async (text: string, at: string): Promise<boolean> => {
    const id = await ensureSession();
    if (!id) return false;
    try {
      await api.schedule(id, at, text);
      await loadSchedules();
      return true;
    } catch (error) {
      fail(error);
      return false;
    }
  };

  const stop = async () => {
    if (!currentId) return;
    try {
      const result = await api.cancel(currentId);
      // Nothing was running any more: the result event is already on its way.
      if (!result.running) conversation.setRunning(false);
    } catch (error) {
      fail(error);
    }
  };

  const patch = async (session: Session, change: Parameters<Api["patchSession"]>[1]) => {
    try {
      mergeSession(await api.patchSession(session.id, change));
    } catch (error) {
      fail(error);
    }
  };

  const rename = (session: Session) => {
    const title = prompt(t("sessionTitle"), session.title || "");
    if (title !== null) patch(session, { title });
  };

  const remove = async (session: Session) => {
    const question = t("confirmDelete", { title: session.title || t("untitled") }) + (session.worktree ? "\n" + t("deleteWorktreeNote", { branch: session.worktree.branch }) : "");
    if (!confirm(question)) return;
    try {
      await api.deleteSession(session.id);
    } catch (error) {
      // Uncommitted work in its worktree: deleting it too needs a second yes.
      if (!(error instanceof HttpError && error.status === 409 && session.worktree && confirm(t("deleteWorktreeDirty")))) {
        fail(error);
        return;
      }
      try {
        await api.deleteSession(session.id, true);
      } catch (again) {
        fail(again);
        return;
      }
    }
    setSessions((list) => list.filter((item) => item.id !== session.id));
    if (currentId === session.id) {
      // Its tab closes; the neighbouring one comes forward.
      const next = neighbourOf(session.id);
      if (next) select(next);
      else setCurrentId(null);
    }
  };

  // The project page acts on several conversations at once.
  const patchMany = async (list: Session[], change: { pinned?: boolean; archived?: boolean }) => {
    for (const session of list) await patch(session, change);
  };
  const deleteMany = async (list: Session[]) => {
    if (!confirm(t("confirmDeleteMany", { n: list.length }))) return;
    const kept: string[] = [];
    for (const session of list) {
      try {
        await api.deleteSession(session.id);
      } catch (error) {
        // Running, or its worktree has uncommitted work: it stays.
        if (error instanceof HttpError && error.status === 409) {
          kept.push(session.title || t("untitled"));
          continue;
        }
        fail(error);
        break;
      }
      setSessions((all) => all.filter((item) => item.id !== session.id));
      if (currentId === session.id) setCurrentId(null);
    }
    if (kept.length) setNotice(t("notDeleted", { list: kept.join(", ") }));
  };

  const toggleSidebar = () => {
    if (narrow()) {
      setMobileSidebar(!mobileSidebar);
      return;
    }
    const next = !collapsed;
    setCollapsed(next);
    writeStorage("ccl-web-sidebar", next ? "collapsed" : "");
  };

  const openPanel = (tab: PanelTab) => {
    const next = panel === tab ? null : tab;
    setPanel(next);
    setPanelWide(false);
    // A phone's choice does not change what the desktop reopens.
    if (!narrowScreen()) writeStorage("ccl-web-panel", next || "closed");
  };
  const togglePanel = () => openPanel(panel || "overview");

  // The open conversation always has a tab.
  useEffect(() => {
    if (currentId) setOpenIds((ids) => (ids.includes(currentId) ? ids : [...ids, currentId]));
  }, [currentId]);
  // Deleted conversations lose their tabs. The list is complete once ready.
  useEffect(() => {
    if (phase !== "ready") return;
    setOpenIds((ids) => {
      const kept = ids.filter((id) => id === currentId || sessions.some((session) => session.id === id));
      return kept.length === ids.length ? ids : kept;
    });
  }, [sessions, phase, currentId]);
  // A background tab's turn: poll the list until it ends, for its spinner.
  const backgroundRunning = sessions.some((session) => session.running && session.id !== currentId && openIds.includes(session.id));
  useEffect(() => {
    if (!backgroundRunning) return;
    const timer = setInterval(loadSessions, 4000);
    return () => clearInterval(timer);
  }, [backgroundRunning, loadSessions]);

  // neighbourOf is the conversation tab next to id (the right one first).
  const neighbourOf = (id: string): string | null => {
    const index = openIds.indexOf(id);
    const rest = openIds.filter((item) => item !== id);
    return rest[Math.min(Math.max(index, 0), rest.length - 1)] ?? null;
  };

  const terminalScope = (tab: TermTab) => (tab.workspaceId ? api.forWorkspace(tab.workspaceId, tab.worktree) : api);
  const newTerminal = () => {
    const key = `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 6)}`;
    const n = termTabs.reduce((max, tab) => Math.max(max, tab.n), 0) + 1;
    setTermTabs((tabs) => [...tabs, { key, id: null, workspaceId: activeWorkspace?.id || "", worktree: worktreeName, n }]);
    setActiveTerm(key);
    setPage("chat");
    setMobileSidebar(false);
  };
  // The bar's terminal button: back to the conversation from a terminal, else
  // the current folder's terminal, opened when there is none.
  const toggleTerminal = () => {
    if (activeTerm) {
      setActiveTerm(null);
      return;
    }
    const own = termTabs.find((tab) => tab.workspaceId === (activeWorkspace?.id || "") && tab.worktree === worktreeName);
    if (own) {
      setActiveTerm(own.key);
      setPage("chat");
    } else newTerminal();
  };
  // openAgent shows a sub-agent of the open conversation in a tab of its own.
  const openAgent = (toolKey: string | null) => {
    setActiveTerm(null);
    setAgentView(toolKey);
    if (!toolKey || !currentId) return;
    const tool = agents.find((agent) => agent.tool.key === toolKey)?.tool;
    const type = tool?.task?.agent || String(tool?.input?.subagent_type || "") || tool?.name || "agent";
    const title = tool?.description || tool?.task?.description || type;
    setAgentTabs((all) => {
      const list = all[currentId] || [];
      return list.some((tab) => tab.key === toolKey) ? all : { ...all, [currentId]: [...list, { key: toolKey, title, type }] };
    });
  };
  const activateTab = (key: string) => {
    if (key.startsWith("a:")) {
      const [sessionId, toolKey] = parseAgentTab(key);
      if (sessionId === currentId) {
        if (page === "project" && !canLeave()) return;
        setPage("chat");
        setActiveTerm(null);
        setAgentView(toolKey);
      } else {
        nextAgent.current = toolKey;
        select(sessionId);
      }
      return;
    }
    if (key === "s:" + currentId) setAgentView(null);
    if (key.startsWith("t:")) {
      setActiveTerm(key.slice(2));
      setPage("chat");
    } else if (key === "new") {
      if (page === "project" && !canLeave()) return;
      setPage("chat");
      setActiveTerm(null);
    } else select(key.slice(2));
  };
  const closeTab = (key: string) => {
    if (key.startsWith("a:")) {
      const [sessionId, toolKey] = parseAgentTab(key);
      setAgentTabs((all) => ({ ...all, [sessionId]: (all[sessionId] || []).filter((tab) => tab.key !== toolKey) }));
      if (sessionId === currentId && agentView === toolKey) setAgentView(null);
      return;
    }
    if (key.startsWith("t:")) {
      const tab = termTabs.find((item) => item.key === key.slice(2));
      if (tab?.id) terminalScope(tab).terminalClose(tab.id).catch(() => {});
      setTermTabs((tabs) => tabs.filter((item) => item.key !== key.slice(2)));
      if (activeTerm === key.slice(2)) setActiveTerm(null);
      return;
    }
    const id = key === "new" ? null : key.slice(2);
    if (id) {
      setOpenIds((ids) => ids.filter((item) => item !== id));
      // Its sub-agent tabs go with it.
      setAgentTabs((all) => {
        const { [id]: _, ...rest } = all;
        return rest;
      });
    }
    // Closing a tab in the background changes nothing in front.
    if (id !== currentId) return;
    const next = id ? neighbourOf(id) : openIds[openIds.length - 1] || null;
    if (next && !activeTerm) {
      select(next);
      return;
    }
    // A terminal in front stays there and the conversation behind it changes.
    setCurrentId(next);
    if (next) return;
    if (activeTerm || termTabs.length) {
      setDraftOpen(false);
      if (!activeTerm) setActiveTerm(termTabs[termTabs.length - 1].key);
    } else startNew();
  };

  // The draft tab shows while no conversation is open, unless it was closed
  // with a terminal in front.
  const showDraft = !currentId && (draftOpen || !activeTerm);
  const tabs: TabItem[] = [
    ...openIds.flatMap((id): TabItem[] => {
      const session = sessions.find((item) => item.id === id);
      const isCurrent = id === currentId;
      const inFront = isCurrent && !activeTerm && page === "chat";
      const subs = agentTabs[id] || [];
      const group = subs.length ? id : undefined;
      return [
        {
          key: "s:" + id,
          kind: "chat",
          group,
          title: session?.title || t("untitled"),
          running: isCurrent ? conversation.running : !!session?.running,
          active: inFront && !viewed,
          closable: true,
        },
        ...subs.map((sub): TabItem => {
          const tool = isCurrent ? agents.find((agent) => agent.tool.key === sub.key)?.tool : undefined;
          return {
            key: agentTabKey(id, sub.key),
            kind: "agent",
            group,
            title: sub.title,
            hint: sub.type,
            running: !!tool && toolActive(tool),
            active: inFront && viewed?.key === sub.key,
            closable: true,
          };
        }),
      ];
    }),
    ...(showDraft ? [{ key: "new", kind: "draft" as const, title: t("newSession"), active: !activeTerm && page === "chat", closable: openIds.length > 0 || termTabs.length > 0 }] : []),
    ...termTabs.map((tab): TabItem => ({
      key: "t:" + tab.key,
      kind: "terminal",
      title: `${t("terminal")} ${tab.n}`,
      hint: [workspaceList.find((item) => item.id === tab.workspaceId)?.name, tab.worktree].filter(Boolean).join(" · "),
      active: tab.key === activeTerm && page === "chat",
      closable: true,
    })),
  ];
  const frontTerm = termTabs.find((tab) => tab.key === activeTerm) || null;

  const mention = useRef<(path: string) => void>(() => {});
  const registerMention = useCallback((insert: (path: string) => void) => {
    mention.current = insert;
  }, []);

  // Files dropped anywhere on the conversation attach to the next message.
  const addFiles = useRef<(files: File[]) => void>(() => {});
  const registerAddFiles = useCallback((add: (files: File[]) => void) => {
    addFiles.current = add;
  }, []);
  const dragDepth = useRef(0);
  const hasFiles = (event: React.DragEvent) => Array.from(event.dataTransfer.types).includes("Files");

  if (phase === "loading") return <div className="h-full bg-canvas" />;
  if (phase === "gate") {
    return (
      <I18n.Provider value={i18n}>
        <Gate
          error={gateError}
          onSubmit={(value) => {
            storeToken(value);
            setGateError("");
            setPhase("loading");
            setToken(value);
          }}
        />
      </I18n.Provider>
    );
  }

  return (
    <I18n.Provider value={i18n}>
      <div className="flex h-full bg-canvas text-ink">
        <div
          className={
            "fixed inset-y-0 left-0 z-20 w-[288px] max-w-[88vw] shadow-xl transition-transform md:static md:z-auto md:translate-x-0 md:shadow-none " +
            (mobileSidebar ? "translate-x-0 " : "-translate-x-full ") +
            (collapsed ? "md:hidden" : "")
          }
        >
          <Sidebar
            sessions={activeWorkspace ? sessions.filter((session) => session.workspace === activeWorkspace.path) : sessions}
            allSessions={sessions}
            recentFolders={recentFolders}
            workspaces={workspaceList}
            workspace={activeWorkspace}
            onWorkspace={chooseWorkspace}
            onRefreshWorkspaces={loadWorkspaces}
            onOpenFolder={openFolder}
            onReopenFolder={canAddWorkspace ? (path) => pickFolder(path).catch(fail) : undefined}
            onRemoveWorkspace={removeWorkspace}
            currentId={page === "chat" ? currentId : null}
            page={page}
            onProjects={showProjects}
            health={health}
            theme={theme}
            onSelect={select}
            onCreate={startNew}
            onRename={rename}
            onDelete={remove}
            onPatch={patch}
            onShare={() => {
              setShareOpen(true);
              setMobileSidebar(false);
            }}
            onExport={(format) => {
              setMobileSidebar(false);
              void exportTranscript(format);
            }}
            onSettings={() => {
              setSettingsOpen(true);
              setMobileSidebar(false);
              loadConfig();
            }}
            onCloud={() => {
              setSettingsOpen("cloud");
              setMobileSidebar(false);
              loadConfig();
            }}
            sync={sync}
            onToggleTheme={toggleTheme}
            onCollapse={toggleSidebar}
            claudeSessions={claudeSessions}
            update={update}
            onLoadClaudeSessions={loadClaudeSessions}
            onImport={importSession}
          />
        </div>
        {mobileSidebar && <div className="fixed inset-0 z-10 bg-black/40 md:hidden" onClick={() => setMobileSidebar(false)} />}

        {page === "projects" && (
          <main className="flex min-w-0 flex-1 flex-col">
            <ProjectsList
              workspaces={workspaceList}
              sessions={sessions}
              current={activeWorkspace}
              recentFolders={recentFolders}
              onOpen={openProject}
              onOpenFolder={openFolder}
              onReopenFolder={canAddWorkspace ? (path) => pickFolder(path).catch(fail) : undefined}
              onRemove={removeWorkspace}
              sidebarHidden={collapsed}
              onShowSidebar={toggleSidebar}
            />
          </main>
        )}
        {page === "project" && (
          <main className="flex min-w-0 flex-1 flex-col">
            {activeWorkspace ? (
              <ProjectDetail
                api={wsApi}
                workspace={activeWorkspace}
                sessions={sessions}
                onBack={showProjects}
                onNewConversation={startNew}
                onSelect={select}
                onPatchMany={patchMany}
                providers={providers}
                defaults={activeDefaults || {}}
                onSaveDefaults={async (defaults) => {
                  if (!activeWorkspace) return;
                  const saved = await api.setProjectDefaults(activeWorkspace.id, defaults);
                  setProjectDefaults((all) => ({ ...all, [activeWorkspace.id]: saved }));
                }}
                onDeleteMany={deleteMany}
                onConnectors={() => {
                  setSettingsOpen("connectors");
                  loadConfig();
                }}
                registerLeaveGuard={registerLeaveGuard}
                sidebarHidden={collapsed}
                onShowSidebar={toggleSidebar}
              />
            ) : null}
          </main>
        )}
        {/* The conversation stays mounted under a page, so a draft survives. */}
        <main
          className={"relative min-w-0 flex-1 flex-col " + (page === "chat" ? "flex" : "hidden")}
          onDragEnter={(event) => {
            if (!hasFiles(event)) return;
            event.preventDefault();
            dragDepth.current++;
            setDragging(true);
          }}
          onDragOver={(event) => hasFiles(event) && event.preventDefault()}
          onDragLeave={() => {
            dragDepth.current = Math.max(0, dragDepth.current - 1);
            if (!dragDepth.current) setDragging(false);
          }}
          onDrop={(event) => {
            if (!hasFiles(event)) return;
            event.preventDefault();
            dragDepth.current = 0;
            setDragging(false);
            addFiles.current(Array.from(event.dataTransfer.files));
          }}
        >
          <TabBar
            tabs={tabs}
            title={frontTerm ? `${t("terminal")} ${frontTerm.n}` : current?.title || t("newSession")}
            onActivate={activateTab}
            onClose={closeTab}
            onNewSession={startNew}
            onNewTerminal={health?.shell ? newTerminal : undefined}
            sidebarHidden={collapsed}
            onShowSidebar={toggleSidebar}
            panel={panel}
            onPanel={openPanel}
            onTerminal={health?.shell ? toggleTerminal : undefined}
            terminalActive={!!frontTerm}
            agentsRunning={agentsRunning}
          />
          <div className="flex min-h-0 flex-1">
            {/* A terminal tab covers the conversation, which stays mounted so its
                attachments and stream survive. Terminals mount only in front. */}
            {frontTerm && health?.shell && (
              <div className="min-w-0 flex-1">
                <Suspense fallback={null}>
                  <TerminalPanel
                    key={frontTerm.key}
                    api={terminalScope(frontTerm)}
                    id={frontTerm.id}
                    onId={(id) => setTermTabs((all) => all.map((tab) => (tab.key === frontTerm.key ? { ...tab, id } : tab)))}
                    onError={fail}
                  />
                </Suspense>
              </div>
            )}
            <div className={"min-w-0 flex-1 flex-col " + (frontTerm && health?.shell ? "hidden" : "flex")}>
              {conversation.error && (
                <div className="mx-6 mt-2 rounded-lg bg-red-500/10 px-3 py-2 text-[13px] text-red-500">{conversation.error}</div>
              )}
              {viewed && <AgentBanner tool={viewed} onBack={() => setAgentView(null)} />}
              <Thread
                key={viewed?.key || "main"}
                items={shownItems}
                running={viewed ? toolActive(viewed) : conversation.running}
                loading={conversation.loading}
                workspace={workspace}
                route={route}
                attachmentURL={(file) => (currentId ? api.uploadURL(currentId, file) : "")}
                onPermission={respondPermission}
                onOnboard={config && !providers.length && !currentId ? () => { setSettingsOpen("providers"); loadConfig(); } : undefined}
              />
              <Composer
                key={!currentId || adopted.current === currentId ? "new" : currentId}
                draftKey={!currentId || adopted.current === currentId ? "new" : currentId}
                running={conversation.running}
                workspace={workspace}
                workspaces={workspaceList}
                workspaceId={activeWorkspace?.id || ""}
                onWorkspace={chooseWorkspace}
                onOpenFolder={openFolder}
                worktree={!currentId && git?.repo ? { on: newWorktree, onToggle: () => setNewWorktree(!newWorktree) } : undefined}
                draft={currentId ? undefined : draftRoute}
                onDraft={setDraftRoute}
                session={current}
                providers={providers}
                capabilities={capabilities}
                onRoute={(change) => current && patch(current, change)}
                onUpload={upload}
                onSend={send}
                onStop={stop}
                registerAddFiles={registerAddFiles}
                registerMention={registerMention}
                onSearchFiles={async (query) => (await wsApi.searchFiles(query)).files}
                queued={queue.map((item) => ({ key: item.key, text: item.text, files: item.attachments.length, steered: item.steered !== undefined }))}
                onSteerQueued={(key) => void steer(key)}
                onRemoveQueued={(key) => currentId && setQueues((all) => ({ ...all, [currentId]: (all[currentId] || []).filter((item) => item.key !== key) }))}
                context={contextOf(conversation.events, current, current ? config?.providers[current.provider]?.contextWindow : undefined)}
                awaitingPermission={pending.length > 0}
                scheduled={schedules}
                onSchedule={scheduleMessage}
                onUnschedule={(id) => currentId && api.unschedule(currentId, id).then(loadSchedules).catch(fail)}
                onManageConnectors={() => {
                  setSettingsOpen("connectors");
                  loadConfig();
                }}
              />
            </div>
            {panel && (
              <>
                <div className="fixed inset-0 z-10 bg-black/30 lg:hidden" onClick={() => openPanel(panel)} />
                <div
                  style={{ width: panel === "overview" ? 290 : panelWide ? "min(760px, 52vw)" : 360 }}
                  className="fixed inset-y-0 right-0 z-20 flex max-w-full shrink-0 flex-col border-l border-line bg-canvas transition-[width] max-lg:!w-[min(560px,100vw)] lg:static lg:z-auto"
                >
                  {/* Below lg the panel is an overlay that covers the screen on a phone, so the
                      backdrop is out of reach: give it its own way back. */}
                  <div className="flex shrink-0 items-center justify-end border-b border-line px-1.5 py-1 lg:hidden">
                    <button
                      onClick={() => openPanel(panel)}
                      className="flex items-center gap-1.5 rounded-md px-2 py-1.5 text-[13px] text-ink-2 hover:bg-hover hover:text-ink"
                    >
                      {t("closePanel")}
                      <PanelRightClose size={16} />
                    </button>
                  </div>
                  {panel === "overview" && (
                    <RightPanel
                      session={current}
                      providers={providers}
                      workspace={workspace}
                      capabilities={capabilities}
                      git={git}
                      api={api}
                      refreshKey={refreshKey}
                      running={conversation.running}
                      onOpenChange={(path) => {
                        setGitFocus(path);
                        setPanel("git");
                      }}
                    />
                  )}
                  {panel === "files" && (
                    <div className="min-h-0 flex-1">
                      <FilesPanel key={scopeKey} api={wsApi} refreshKey={refreshKey} onMention={(path) => mention.current(path)} onWide={setPanelWide} onError={fail} onSaved={loadGit} />
                    </div>
                  )}
                  {panel === "agents" && <AgentsPanel items={items} running={conversation.running} selected={viewed ? agentView : null} onSelect={openAgent} />}
                  {panel === "git" && (
                    <div className="min-h-0 flex-1">
                      <GitPanel key={scopeKey} api={wsApi} status={git} onStatus={setGit} onRefresh={loadGit} focus={gitFocus} onWide={setPanelWide} onError={fail} />
                    </div>
                  )}
                </div>
              </>
            )}
          </div>
          {dragging && (
            <div className="pointer-events-none absolute inset-3 z-30 grid place-items-center rounded-2xl border-2 border-dashed border-clay/70 bg-canvas/80">
              <div className="flex items-center gap-2 text-[15px] text-ink">
                <Paperclip size={18} className="text-clay" /> {t("dropFiles")}
              </div>
            </div>
          )}
        </main>

        {paletteOpen && (
          <CommandPalette
            actions={[
              { id: "new", label: t("newSession"), icon: <Plus size={15} />, run: startNew },
              { id: "projects", label: t("projects"), icon: <Folders size={15} />, run: showProjects },
              { id: "settings", label: t("settings"), icon: <Settings size={15} />, run: () => { setSettingsOpen(true); loadConfig(); } },
              { id: "theme", label: theme === "dark" ? t("lightTheme") : t("darkTheme"), icon: theme === "dark" ? <Sun size={15} /> : <Moon size={15} />, run: toggleTheme },
              { id: "sidebar", label: t("toggleSidebar"), icon: <SidebarIcon size={15} />, run: toggleSidebar },
              { id: "panel", label: t("togglePanel"), icon: <PanelRight size={15} />, run: togglePanel },
              ...(health?.shell ? [{ id: "terminal", label: t("terminalNew"), icon: <SquareTerminal size={15} />, run: newTerminal }] : []),
            ]}
            sessions={sessions}
            claudeSessions={claudeSessions || []}
            onSelect={select}
            onImport={importSession}
            onClose={() => setPaletteOpen(false)}
          />
        )}

        {shareOpen && current && (
          <ShareDialog api={api} sessionId={current.id} token={current.shareToken} onChanged={loadSessions} onClose={() => setShareOpen(false)} />
        )}

        {folderPicker && (
          <FolderPicker
            api={api}
            start={activeWorkspace?.path.replace(/[\\/][^\\/]+[\\/]?$/, "") || ""}
            create={folderPicker === "create"}
            onPick={pickFolder}
            onClose={() => setFolderPicker(false)}
          />
        )}

        {settingsOpen && (
          <SettingsDialog initialTab={settingsOpen} api={api} workspaceApi={wsApi} config={config} loadError={configError} onClose={() => {
            setSettingsOpen(false);
            // Settings may have pushed, pulled or logged in.
            void loadSync();
          }} onChanged={loadConfig} />
        )}

        {notice && (
          <div className="fixed bottom-24 left-1/2 z-40 flex max-w-[90vw] -translate-x-1/2 items-center gap-3 rounded-xl border border-line bg-surface px-4 py-2.5 text-[13px] text-ink shadow-xl">
            <span className="truncate">{notice}</span>
            <button onClick={() => setNotice("")} aria-label={t("dismiss")} className="text-ink-3 hover:text-ink">
              <X size={15} />
            </button>
          </div>
        )}
      </div>
    </I18n.Provider>
  );
}
