// Wire types of the ccl web API (internal/webui). Field names follow the Go
// JSON tags.

export interface Usage {
  inputTokens: number;
  outputTokens: number;
  costUsd: number;
  durationMs: number;
}

export interface Session {
  id: string;
  title: string;
  provider: string;
  model: string;
  effort: string;
  permissionMode?: string;
  claudeSessionId?: string;
  workspace: string;
  // Set when the conversation runs on a branch of its own in a git worktree.
  worktree?: SessionWorktree;
  // Where the page lists it: pinned on top, archived out of the recents.
  pinned?: boolean;
  archived?: boolean;
  createdAt: string;
  updatedAt: string;
  turns: number;
  usage: Usage;
  claudeCostUsd?: number;
  // Known only to the running server; never stored.
  running?: boolean;
}

export interface StoredEvent {
  seq: number;
  type: string;
  payload: unknown;
  // Milliseconds since the epoch. Live stream events carry the arrival time.
  ts: number;
}

export interface Attachment {
  id: string;
  name: string;
  type: string;
  size: number;
  // Sent to the model as an image block; other files are passed by path.
  image: boolean;
}

export interface UserPayload {
  text: string;
  attachments?: Attachment[];
}

export interface MCPServer {
  name: string;
  status: string;
}

export interface Capabilities {
  slashCommands: string[];
  mcpServers: MCPServer[];
  // The mode a session without its own uses (follows ccl's bypass setting).
  defaultPermissionMode?: string;
}

export interface InitPayload {
  sessionId: string;
  model?: string;
  cwd?: string;
  slashCommands?: string[];
  mcpServers?: MCPServer[];
}

export interface TokenPayload {
  text: string;
  subagent?: boolean;
  // The Task call a sub-agent's event belongs to.
  parent?: string;
}

export interface ToolUsePayload {
  id: string;
  name: string;
  summary: string;
  description?: string;
  // Arguments with credentials masked and long strings cut.
  input?: unknown;
  parent?: string;
  subagent?: boolean;
}

export interface ToolResultPayload {
  id: string;
  parent?: string;
  name?: string;
  ok: boolean;
  output: string;
  subagent?: boolean;
}

export interface ResultPayload {
  // Claude Code's report: costUsd is the session's running total.
  usage: Usage;
  // What this turn alone spent; absent when Claude Code reported nothing.
  turnUsage?: Usage;
  isError: boolean;
  cancelled?: boolean;
  message?: string;
}

export interface Health {
  ok: boolean;
  version: string;
  claudeVersion: string;
  sessions: number;
  // The default workspace; workspaces lists every one the server offers.
  workspace: string;
  workspaces?: Workspace[];
  authRequired: boolean;
  // The web terminal is on (ccl web --shell).
  shell?: boolean;
}

export interface ModelChoice {
  value: string;
  label: string;
}

export interface ProviderView {
  name: string;
  type: string;
  endpoint: string;
  apiKey: string;
  hasKey: boolean;
  oauthProvider?: string;
  opusModel?: string;
  sonnetModel?: string;
  haikuModel?: string;
  fableModel?: string;
  customModelId?: string;
  subagentModel?: string;
  effortLevel?: string;
  // The model pool, comma-separated.
  models?: string;
  active: boolean;
  acpActive: boolean;
  modelChoices: ModelChoice[];
}

export interface ConfigView {
  active: { normal: string; acp: string };
  lang: string;
  bypassMode: boolean;
  providers: Record<string, ProviderView>;
}

export interface TestResult {
  ok: boolean;
  elapsedMs: number;
  models?: string[];
  message?: string;
}

export const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;

export interface FileEntry {
  name: string;
  path: string;
  type: "file" | "dir" | "symlink";
  size: number;
  modified: string;
}

export interface FileContent {
  path: string;
  size: number;
  modified: string;
  hash: string;
  type: string;
  binary: boolean;
  image: boolean;
  tooBig?: boolean;
  content?: string;
}

export interface GitFile {
  path: string;
  origPath?: string;
  index: string;
  worktree: string;
  untracked?: boolean;
  conflicted?: boolean;
  added: number;
  deleted: number;
}

export interface GitStatus {
  repo: boolean;
  branch?: string;
  detached?: boolean;
  upstream?: string;
  ahead: number;
  behind: number;
  files: GitFile[];
  added: number;
  deleted: number;
  error?: string;
}

export interface GitBranch {
  name: string;
  current: boolean;
  upstream?: string;
  date?: string;
}

export interface GitCommit {
  hash: string;
  short: string;
  author: string;
  date: string;
  subject: string;
}

export interface ClaudeSession {
  id: string;
  title: string;
  updated: string;
  size: number;
  prompts: number;
  webSession?: string;
}

export const PERMISSION_MODES = ["default", "acceptEdits", "plan", "bypassPermissions"] as const;

export interface PermissionDecision {
  requestId: string;
  behavior: "allow" | "deny";
  message?: string;
  answers?: Record<string, string>;
  remember?: boolean;
  mode?: string;
}

export interface MCPEntry {
  name: string;
  scope: "user" | "project" | "local";
  type: string;
  target: string;
  env?: string[];
}

export interface Schedule {
  id: string;
  session: string;
  at: string;
  text: string;
}

export const OAUTH_PROVIDERS = ["gpt", "gemini", "grok", "qoder", "kimi", "kiro", "workbuddy", "workbuddycn", "autoclaw", "zed"] as const;

export interface SessionWorktree {
  name: string;
  branch: string;
  // The branch it started from; absent when the project's HEAD was detached.
  base?: string;
  baseCommit: string;
}

export interface WorktreeStatus extends SessionWorktree {
  path: string;
  exists: boolean;
  ahead: number;
  behind: number;
  dirty: number;
  files: { path: string; status: string }[];
  // The branch the project itself has checked out; merges go there.
  current?: string;
  remote?: string;
  upstream?: string;
}

export interface Workspace {
  id: string;
  name: string;
  path: string;
  // A folder added from the page; only those can be removed from the list.
  added?: boolean;
}

// One folder listing for the folder picker.
export interface DirListing {
  path: string;
  parent: string;
  home: string;
  repo: boolean;
  entries: { name: string; path: string; repo?: boolean }[];
}
