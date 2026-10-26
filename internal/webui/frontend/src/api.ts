import type {
  Attachment,
  Capabilities,
  ClaudeSession,
  CloudOverview,
  ConfigView,
  DirListing,
  FileContent,
  FileEntry,
  GitBranch,
  GitCommit,
  GitStatus,
  Health,
  MCPEntry,
  PermissionDecision,
  Schedule,
  Session,
  StoredEvent,
  TestResult,
  ProjectDefaults,
  Workspace,
  WorktreeDiff,
  WorktreeStatus,
} from "./types";
import { appURL } from "./base";

// The token is needed only when ccl serve listens beyond 127.0.0.1. It stays in
// this browser's storage and nowhere else.
const TOKEN_KEY = "ccl-web-token";

// The page sends its token in X-CCL-Token, never Authorization: a proxy in
// front of ccl serve may need that header for its own Basic auth.
export const TOKEN_HEADER = "X-CCL-Token";

export function storedToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) || "";
  } catch {
    return "";
  }
}

export function storeToken(token: string) {
  try {
    if (token) localStorage.setItem(TOKEN_KEY, token);
    else localStorage.removeItem(TOKEN_KEY);
  } catch {
    // Not persisted: the gate asks again next time.
  }
}

export class Unauthorized extends Error {
  constructor() {
    super("Token rejected");
  }
}

// HttpError keeps the status, for callers that treat one (a 404) as an answer.
export class HttpError extends Error {
  constructor(
    public status: number,
    message: string,
    // The parsed response, for errors that carry more than a message.
    public body?: unknown,
  ) {
    super(message);
  }
}

// Requests about a workspace's files, git, MCP servers, Claude Code sessions
// or terminals name the workspace they mean.
const WORKSPACE_SCOPED = ["/api/files/", "/api/git/", "/api/claude-sessions", "/api/mcp", "/api/terminal"];

export class Api {
  constructor(
    public token: string,
    public workspace = "",
    // A conversation's worktree inside the workspace.
    public worktree = "",
  ) {}

  // forWorkspace is this client scoped to one workspace, or to one of its
  // conversation worktrees.
  forWorkspace(id: string, worktree = ""): Api {
    return new Api(this.token, id, worktree);
  }

  scoped(path: string): string {
    if (!this.workspace || !WORKSPACE_SCOPED.some((prefix) => path.startsWith(prefix))) return path;
    const scope = "workspace=" + encodeURIComponent(this.workspace) + (this.worktree ? "&worktree=" + encodeURIComponent(this.worktree) : "");
    return path + (path.includes("?") ? "&" : "?") + scope;
  }

  async request<T>(path: string, options: { method?: string; body?: unknown } = {}): Promise<T> {
    path = this.scoped(path);
    const headers: Record<string, string> = {};
    if (this.token) headers[TOKEN_HEADER] = this.token;
    if (options.body !== undefined) headers["Content-Type"] = "application/json";
    const response = await fetch(appURL(path), {
      method: options.method || "GET",
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
    });
    if (response.status === 401) throw new Unauthorized();
    const text = await response.text();
    let body: unknown = null;
    if (text) {
      try {
        body = JSON.parse(text);
      } catch {
        body = text;
      }
    }
    if (!response.ok) {
      const message = body && typeof body === "object" && "error" in body ? String(body.error) : response.statusText;
      throw new HttpError(response.status, message, body);
    }
    return body as T;
  }

  health = () => fetch(appURL("/health")).then((response) => response.json() as Promise<Health>);

  sessions = () => this.request<Session[]>("/api/sessions");
  session = (id: string) => this.request<Session>(`/api/sessions/${id}`);
  createSession = (workspace = "", worktree = false, route: ProjectDefaults = {}) =>
    this.request<Session>("/api/sessions", { method: "POST", body: { ...route, ...(workspace ? { workspace } : {}), ...(worktree ? { worktree } : {}) } });
  setProjectDefaults = (id: string, defaults: ProjectDefaults) =>
    this.request<ProjectDefaults>(`/api/workspaces/${encodeURIComponent(id)}/defaults`, { method: "PUT", body: defaults });
  worktreeDiff = (id: string) => this.request<WorktreeDiff>(`/api/sessions/${id}/worktree/diff`);
  share = (id: string) => this.request<{ token: string; path: string }>(`/api/sessions/${id}/share`, { method: "POST", body: {} });
  unshare = (id: string) => this.request<null>(`/api/sessions/${id}/share`, { method: "DELETE" });
  worktreeStatus = (id: string) => this.request<WorktreeStatus>(`/api/sessions/${id}/worktree`);
  worktreeAction = (id: string, action: "merge" | "push") =>
    this.request<{ output: string; status: WorktreeStatus }>(`/api/sessions/${id}/worktree/${action}`, { method: "POST", body: {} });
  workspaces = () => this.request<{ workspaces: Workspace[]; canAdd: boolean; defaults?: Record<string, ProjectDefaults> }>("/api/workspaces");
  addWorkspace = (path: string) => this.request<Workspace>("/api/workspaces", { method: "POST", body: { path } });
  removeWorkspace = (id: string) => this.request<null>(`/api/workspaces/${encodeURIComponent(id)}`, { method: "DELETE" });
  createDirectory = (path: string, name: string) => this.request<{ path: string }>("/api/directories", { method: "POST", body: { path, name } });
  directories = (path = "", hidden = false) =>
    this.request<DirListing>(`/api/directories?path=${encodeURIComponent(path)}${hidden ? "&hidden=1" : ""}`);
  patchSession = (id: string, patch: Partial<Pick<Session, "title" | "provider" | "model" | "effort" | "permissionMode" | "pinned" | "archived">>) =>
    this.request<Session>(`/api/sessions/${id}`, { method: "PATCH", body: patch });
  deleteSession = (id: string, force = false) => this.request<null>(`/api/sessions/${id}${force ? "?force=1" : ""}`, { method: "DELETE" });
  events = (id: string, since = 0) => this.request<StoredEvent[]>(`/api/sessions/${id}/events?since=${since}`);
  // steer sends the message into the running turn; with none running it
  // starts one like any message.
  send = (id: string, text: string, attachments: string[] = [], steer = false) =>
    this.request<{ seq?: number; running: boolean; steered?: boolean }>(`/api/sessions/${id}/messages`, { method: "POST", body: steer ? { text, attachments, steer } : { text, attachments } });

  async upload(id: string, files: File[]): Promise<Attachment[]> {
    const form = new FormData();
    for (const file of files) form.append("file", file, file.name);
    const headers: Record<string, string> = {};
    if (this.token) headers[TOKEN_HEADER] = this.token;
    const response = await fetch(appURL(`/api/sessions/${id}/uploads`), { method: "POST", headers, body: form });
    if (response.status === 401) throw new Unauthorized();
    const body = await response.json().catch(() => null);
    if (!response.ok) throw new Error((body && body.error) || response.statusText);
    return body as Attachment[];
  }

  // <img> cannot set a header either; attachments take the token as a query.
  uploadURL = (id: string, file: string) =>
    appURL(`/api/sessions/${id}/uploads/${encodeURIComponent(file)}` + (this.token ? `?token=${encodeURIComponent(this.token)}` : ""));

  capabilities = () => this.request<Capabilities>("/api/capabilities");

  searchFiles = (query: string) => this.request<{ files: string[] }>(`/api/files/search?q=${encodeURIComponent(query)}`);
  listFiles = (path: string) => this.request<{ path: string; entries: FileEntry[] }>(`/api/files/list?path=${encodeURIComponent(path)}`);
  readFile = (path: string) => this.request<FileContent>(`/api/files/read?path=${encodeURIComponent(path)}`);
  writeFile = (path: string, content: string, expectedHash: string) =>
    this.request<{ path: string; hash: string }>("/api/files/write", { method: "PUT", body: { path, content, expectedHash } });
  rawFileURL = (path: string) =>
    appURL(this.scoped(`/api/files/raw?path=${encodeURIComponent(path)}`) + (this.token ? `&token=${encodeURIComponent(this.token)}` : ""));

  async uploadToWorkspace(dir: string, files: File[]): Promise<string[]> {
    const form = new FormData();
    for (const file of files) form.append("file", file, file.name);
    const headers: Record<string, string> = {};
    if (this.token) headers[TOKEN_HEADER] = this.token;
    const response = await fetch(appURL(this.scoped(`/api/files/uploads?dir=${encodeURIComponent(dir)}`)), { method: "POST", headers, body: form });
    if (response.status === 401) throw new Unauthorized();
    const body = await response.json().catch(() => null);
    if (!response.ok) throw new Error((body && body.error) || response.statusText);
    return (body as { files: string[] }).files;
  }

  gitStatus = () => this.request<GitStatus>("/api/git/status");
  gitDiff = (path: string, options: { staged?: boolean; untracked?: boolean } = {}) =>
    this.request<{ diff: string; truncated: boolean }>(
      `/api/git/diff?path=${encodeURIComponent(path)}${options.staged ? "&staged=1" : ""}${options.untracked ? "&untracked=1" : ""}`,
    );
  gitStage = (paths: string[]) => this.request<GitStatus>("/api/git/stage", { method: "POST", body: { paths } });
  gitUnstage = (paths: string[]) => this.request<GitStatus>("/api/git/unstage", { method: "POST", body: { paths } });
  gitCommit = (message: string) => this.request<GitStatus>("/api/git/commit", { method: "POST", body: { message } });
  gitBranches = () => this.request<{ branches: GitBranch[] }>("/api/git/branches");
  gitCheckout = (branch: string, create = false) => this.request<GitStatus>("/api/git/checkout", { method: "POST", body: { branch, create } });
  gitLog = (limit = 50) => this.request<{ commits: GitCommit[] }>(`/api/git/log?limit=${limit}`);
  gitWorktrees = () => this.request<{ worktrees: { path: string; branch?: string; head?: string; detached?: boolean; current?: boolean }[] }>("/api/git/worktrees");
  gitShow = (commit: string) => this.request<{ diff: string; truncated: boolean }>(`/api/git/show?commit=${encodeURIComponent(commit)}`);

  oauthStart = (provider: string, alias: string) =>
    this.request<{ id: string }>("/api/oauth/login", { method: "POST", body: { provider, alias } });
  oauthStatus = (id: string) =>
    this.request<{ id: string; provider: string; output: string[]; urls: string[]; done: boolean; ok: boolean; cancelled?: boolean; error?: string }>(`/api/oauth/login/${id}`);
  oauthCancel = (id: string) => this.request<null>(`/api/oauth/login/${id}`, { method: "DELETE" });

  mcpList = () => this.request<{ servers: MCPEntry[]; claude: boolean }>("/api/mcp");
  mcpAdd = (body: Record<string, unknown>) => this.request<{ servers: MCPEntry[] }>("/api/mcp", { method: "POST", body });
  mcpRemove = (name: string, scope: string) =>
    this.request<{ servers: MCPEntry[] }>(`/api/mcp/${encodeURIComponent(name)}?scope=${encodeURIComponent(scope)}`, { method: "DELETE" });

  userMemory = () => this.request<{ path: string; hash: string; content: string; tooBig?: boolean }>("/api/memory");
  saveUserMemory = (content: string, expectedHash: string) =>
    this.request<{ path: string; hash: string }>("/api/memory", { method: "PUT", body: { content, expectedHash } });
  cloud = () => this.request<CloudOverview>("/api/cloud");
  cloudCommand = (action: "push" | "pull" | "primary" | "logout", remote: string, force = false) =>
    this.request<{ output: string }>(`/api/cloud/${action}`, { method: "POST", body: { remote, force } });
  cloudLogin = (platform: string, alias: string) => this.request<{ id: string }>("/api/cloud/login", { method: "POST", body: { platform, alias } });
  cloudLoginStatus = (id: string) =>
    this.request<{ id: string; output: string[]; urls: string[]; done: boolean; ok: boolean; cancelled?: boolean; error?: string }>(`/api/cloud/login/${id}`);
  cloudLoginPaste = (id: string, address: string) => this.request<null>(`/api/cloud/login/${id}`, { method: "POST", body: { address } });
  cloudLoginCancel = (id: string) => this.request<null>(`/api/cloud/login/${id}`, { method: "DELETE" });
  updateInfo = () => this.request<{ current: string; latest: string; newer: boolean }>("/api/update");

  schedules = (id: string) => this.request<{ schedules: Schedule[] }>(`/api/sessions/${id}/schedules`);
  schedule = (id: string, at: string, text: string) => this.request<Schedule>(`/api/sessions/${id}/schedules`, { method: "POST", body: { at, text } });
  unschedule = (id: string, scheduleId: string) => this.request<null>(`/api/sessions/${id}/schedules/${scheduleId}`, { method: "DELETE" });

  terminalOpen = (cols: number, rows: number) => this.request<{ id: string }>("/api/terminal", { method: "POST", body: { cols, rows } });
  terminalInput = (id: string, data: string) => this.request<null>(`/api/terminal/${id}/input`, { method: "POST", body: { data } });
  terminalResize = (id: string, cols: number, rows: number) =>
    this.request<null>(`/api/terminal/${id}/resize`, { method: "POST", body: { cols, rows } });
  terminalClose = (id: string) => this.request<null>(`/api/terminal/${id}`, { method: "DELETE" });
  terminalStreamURL = (id: string, since: number) =>
    appURL(`/api/terminal/${id}/stream?since=${since}` + (this.token ? `&token=${encodeURIComponent(this.token)}` : ""));

  claudeSessions = () => this.request<{ sessions: ClaudeSession[] }>("/api/claude-sessions");
  importClaudeSession = (id: string) => this.request<Session>("/api/claude-sessions/import", { method: "POST", body: { id } });
  respondPermission = (id: string, decision: PermissionDecision) =>
    this.request<{ ok: boolean }>(`/api/sessions/${id}/permissions`, { method: "POST", body: decision });
  cancel = (id: string) =>
    this.request<{ cancelled: boolean; running: boolean }>(`/api/sessions/${id}/cancel`, { method: "POST", body: {} });

  // EventSource cannot set a header, so the stream takes the token as a query.
  streamURL = (id: string, since: number) =>
    appURL(`/api/sessions/${id}/stream?since=${since}` + (this.token ? `&token=${encodeURIComponent(this.token)}` : ""));

  config = () => this.request<ConfigView>("/api/config");
  putConfig = (body: { active?: { normal: string; acp: string }; lang?: string; bypassMode?: boolean }) =>
    this.request<ConfigView>("/api/config", { method: "PUT", body });
  createProvider = (body: Record<string, unknown>) =>
    this.request<unknown>("/api/config/providers", { method: "POST", body });
  patchProvider = (name: string, body: Record<string, unknown>) =>
    this.request<unknown>(`/api/config/providers/${encodeURIComponent(name)}`, { method: "PATCH", body });
  deleteProvider = (name: string) =>
    this.request<unknown>(`/api/config/providers/${encodeURIComponent(name)}`, { method: "DELETE" });
  testProvider = (name: string, body: Record<string, unknown> = {}) =>
    this.request<TestResult>(`/api/config/providers/${encodeURIComponent(name)}/test`, { method: "POST", body });
}
