import type {
  Attachment,
  Capabilities,
  InitPayload,
  ResultPayload,
  StoredEvent,
  TaskPayload,
  TokenPayload,
  ToolResultPayload,
  ToolUsePayload,
  UserPayload,
} from "./types";
import { diffLines, diffStats } from "./diff";

// A conversation's history is a flat event stream; the thread is what the page
// renders from it. buildThread is pure so it can be tested and recomputed on
// every streamed token. A sub-agent's events nest under the Task call that
// started it.

export type TextItem = { kind: "text"; key: string; text: string; subagent: boolean };
export type ThinkingItem = { kind: "thinking"; key: string; text: string };

export type ToolItem = {
  kind: "tool";
  key: string;
  id: string;
  name: string;
  summary: string;
  description: string;
  input?: Record<string, unknown>;
  subagent: boolean;
  status: "running" | "ok" | "err";
  output?: string;
  // What a sub-agent did inside this call (Task / Agent).
  children: ThreadItem[];
  // The background work the call started. Its tool_result only says the work
  // was launched; this says whether it still runs and how it ended.
  task?: Omit<TaskPayload, "id">;
  // When the call started and, for background work, when that ended (ms).
  startedAt: number;
  endedAt?: number;
  // The sub-agent call this one was made in.
  parent?: ToolItem;
};

export type PermissionItem = {
  kind: "permission";
  key: string;
  requestId: string;
  toolName: string;
  title: string;
  description: string;
  summary: string;
  reason: string;
  input?: Record<string, unknown>;
  canRemember: boolean;
  status: "pending" | "allow" | "deny" | "cancelled";
  message: string;
  mode: string;
};

export type ThreadItem =
  // A day separator; the label is rendered in the page's language.
  | { kind: "day"; key: string; ts: number }
  | { kind: "user"; key: string; text: string; attachments: Attachment[]; ts: number }
  | TextItem
  | ThinkingItem
  | ToolItem
  | { kind: "error"; key: string; message: string }
  | { kind: "queued"; key: string }
  | PermissionItem
  | { kind: "turn-end"; key: string; result: ResultPayload; model?: string; ts: number };

interface Scope {
  items: ThreadItem[];
  text: TextItem | null;
  thinking: ThinkingItem | null;
  lastTool: ToolItem | null;
}

const scope = (items: ThreadItem[]): Scope => ({ items, text: null, thinking: null, lastTool: null });

export function buildThread(events: StoredEvent[]): ThreadItem[] {
  const root = scope([]);
  // Tool ids are only meaningful within a turn (some providers reuse them).
  // A background sub-agent can report after the turn that started it, so its
  // events fall back to the latest earlier call with that id.
  let tools = new Map<string, ToolItem>();
  const earlier = new Map<string, ToolItem>();
  const scopes = new Map<ToolItem, Scope>();
  const callOf = (id: string) => tools.get(id) || earlier.get(id);
  const permissions = new Map<string, PermissionItem>();
  let model: string | undefined;
  let lastDay = "";

  // A sub-agent's event goes into its Task call when that call is known.
  const scopeFor = (parent?: string): Scope => {
    if (!parent) return root;
    const tool = callOf(parent);
    if (!tool) return root;
    let child = scopes.get(tool);
    if (!child) {
      child = scope(tool.children);
      scopes.set(tool, child);
    }
    return child;
  };

  for (const event of events) {
    const payload = (event.payload || {}) as Record<string, unknown>;
    const parent = typeof payload.parent === "string" ? payload.parent : undefined;
    switch (event.type) {
      case "user": {
        const user = payload as unknown as UserPayload;
        const day = event.ts ? new Date(event.ts).toDateString() : "";
        if (day && day !== lastDay) {
          lastDay = day;
          root.items.push({ kind: "day", key: `d${event.seq}`, ts: event.ts });
        }
        root.items.push({ kind: "user", key: `u${event.seq}`, text: String(user.text || ""), attachments: user.attachments || [], ts: event.ts });
        for (const [id, tool] of tools) earlier.set(id, tool);
        tools = new Map();
        root.text = root.thinking = root.lastTool = null;
        model = undefined;
        break;
      }
      case "init":
        model = (payload as unknown as InitPayload).model || model;
        // A later init in the same turn starts Claude Code's answer to
        // background work that finished: its text is a new paragraph.
        root.text = root.thinking = null;
        break;
      case "thinking": {
        const target = scopeFor(parent);
        if (!target.thinking) {
          target.thinking = { kind: "thinking", key: `k${event.seq}`, text: "" };
          target.items.push(target.thinking);
          target.text = null;
        }
        target.thinking.text += (payload as unknown as TokenPayload).text || "";
        break;
      }
      case "token": {
        const token = payload as unknown as TokenPayload;
        const target = scopeFor(parent);
        // Legacy events mark sub-agent text without naming its call.
        const subagent = !!token.subagent && target === root;
        if (!target.text || target.text.subagent !== subagent) {
          target.text = { kind: "text", key: `t${event.seq}`, text: "", subagent };
          target.items.push(target.text);
        }
        target.thinking = null;
        target.text.text += token.text || "";
        break;
      }
      case "tool_use": {
        const use = payload as unknown as ToolUsePayload;
        const target = scopeFor(parent);
        target.text = target.thinking = null;
        // A call is announced as it starts streaming and again, with its input,
        // once the assistant message is complete: one row for both.
        let tool = use.id ? tools.get(use.id) : undefined;
        if (!tool) {
          tool = {
            kind: "tool",
            key: `c${event.seq}`,
            id: use.id || "",
            name: use.name || "tool",
            summary: "",
            description: "",
            subagent: !!use.subagent && target === root,
            status: "running",
            children: [],
            startedAt: event.ts,
            parent: parent ? callOf(parent) : undefined,
          };
          if (use.id) tools.set(use.id, tool);
          target.items.push(tool);
        }
        if (use.name) tool.name = use.name;
        if (use.summary) tool.summary = use.summary;
        if (use.description) tool.description = use.description;
        if (use.input && typeof use.input === "object") tool.input = use.input as Record<string, unknown>;
        target.lastTool = tool;
        break;
      }
      case "tool_result": {
        const result = payload as unknown as ToolResultPayload;
        const target = scopeFor(parent);
        const tool = (result.id && (parent ? callOf(result.id) : tools.get(result.id))) || target.lastTool;
        if (tool) {
          tool.status = result.ok === false ? "err" : "ok";
          tool.output = result.output || "";
        }
        target.lastTool = null;
        break;
      }
      case "task": {
        const { id, ...task } = payload as unknown as TaskPayload;
        const tool = id ? callOf(id) : undefined;
        if (tool) {
          tool.task = { ...tool.task, ...task, summary: task.summary || tool.task?.summary, usage: task.usage || tool.task?.usage };
          tool.endedAt = task.status === "running" ? undefined : event.ts;
        }
        break;
      }
      case "permission": {
        root.text = root.thinking = null;
        const request = payload as Record<string, unknown>;
        const item: PermissionItem = {
          kind: "permission",
          key: `p${event.seq}`,
          requestId: String(request.requestId || ""),
          toolName: String(request.toolName || ""),
          title: String(request.title || ""),
          description: String(request.description || ""),
          summary: String(request.summary || ""),
          reason: String(request.reason || ""),
          input: request.input && typeof request.input === "object" ? (request.input as Record<string, unknown>) : undefined,
          canRemember: !!request.canRemember,
          status: "pending",
          message: "",
          mode: "",
        };
        permissions.set(item.requestId, item);
        root.items.push(item);
        break;
      }
      case "permission_resolved": {
        const resolved = payload as Record<string, unknown>;
        const item = permissions.get(String(resolved.requestId || ""));
        if (item) {
          item.status = (["allow", "deny", "cancelled"].includes(String(resolved.behavior)) ? resolved.behavior : "cancelled") as PermissionItem["status"];
          item.message = String(resolved.message || "");
          item.mode = String(resolved.mode || "");
        }
        break;
      }
      case "error":
        root.text = root.thinking = null;
        root.items.push({ kind: "error", key: `e${event.seq}`, message: String(payload.message || "error") });
        break;
      case "queued":
      case "status":
        if (event.type === "queued" || payload.state === "queued") {
          root.text = null;
          root.items.push({ kind: "queued", key: `q${event.seq}` });
        }
        break;
      case "result":
        root.text = root.thinking = root.lastTool = null;
        // A tool still marked running when its turn ended never reported back,
        // and a question nobody answered can no longer be answered. Background
        // work outlives a turn that ended normally (one continued in a
        // terminal); a stopped or failed turn ends it.
        const ended = !!(payload.cancelled || payload.isError);
        for (const tool of tools.values()) {
          if (tool.status === "running" && (ended || !inRunningAgent(tool))) tool.status = "err";
          if (ended && tool.task?.status === "running") tool.task.status = "stopped";
        }
        for (const item of permissions.values()) {
          if (item.status === "pending") item.status = "cancelled";
        }
        root.items.push({ kind: "turn-end", key: `r${event.seq}`, result: payload as unknown as ResultPayload, model, ts: event.ts });
        break;
    }
  }
  return root.items;
}

// latestCapabilities is what the newest init event of a conversation offered.
export function latestCapabilities(events: StoredEvent[]): Capabilities | null {
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].type !== "init") continue;
    const init = events[i].payload as InitPayload;
    return { slashCommands: init.slashCommands || [], mcpServers: init.mcpServers || [] };
  }
  return null;
}

// A call is working while it runs or while the background work it started does.
export function toolActive(tool: ToolItem): boolean {
  return tool.status === "running" || tool.task?.status === "running";
}

// A call made inside a sub-agent that still works is not over when the turn
// that started the sub-agent is.
function inRunningAgent(tool: ToolItem): boolean {
  for (let owner = tool.parent; owner; owner = owner.parent) {
    if (owner.task?.status === "running") return true;
  }
  return false;
}

export const isAgent = (tool: ToolItem) => tool.name === "Agent" || tool.name === "Task" || tool.children.length > 0;

// agentsOf lists a conversation's sub-agents in order, each with how deeply
// it is nested in another one.
export function agentsOf(items: ThreadItem[], depth = 0): { tool: ToolItem; depth: number }[] {
  const out: { tool: ToolItem; depth: number }[] = [];
  for (const item of items) {
    if (item.kind !== "tool") continue;
    const agent = isAgent(item);
    if (agent) out.push({ tool: item, depth });
    out.push(...agentsOf(item.children, agent ? depth + 1 : depth));
  }
  return out;
}

// agentThread is a sub-agent's own conversation, for the main view: the
// prompt it was given, then what it did.
export function agentThread(tool: ToolItem): ThreadItem[] {
  const prompt = typeof tool.input?.prompt === "string" ? tool.input.prompt : "";
  const head: ThreadItem[] = prompt ? [{ kind: "user", key: `${tool.key}-prompt`, text: prompt, attachments: [], ts: tool.startedAt }] : [];
  return [...head, ...tool.children];
}

// The latest step of a sub-agent, for its call's one-line preview.
export function latestStep(tool: ToolItem): string {
  for (let i = tool.children.length - 1; i >= 0; i--) {
    const child = tool.children[i];
    if (child.kind === "tool") return child.description || [child.name, child.summary].filter(Boolean).join(" ");
  }
  return tool.task?.summary || tool.task?.lastTool || "";
}

const str = (value: unknown) => (typeof value === "string" ? value : "");

// The before/after text of an editing call, or null for other tools.
export function toolEdit(tool: ToolItem): { path: string; before: string; after: string }[] | null {
  const input = tool.input;
  if (!input) return null;
  const path = str(input.file_path) || str(input.notebook_path);
  switch (tool.name) {
    case "Edit":
      return [{ path, before: str(input.old_string), after: str(input.new_string) }];
    case "MultiEdit":
      return Array.isArray(input.edits)
        ? input.edits.map((edit) => ({ path, before: str((edit as Record<string, unknown>).old_string), after: str((edit as Record<string, unknown>).new_string) }))
        : null;
    case "Write":
      return [{ path, before: "", after: str(input.content) }];
    case "NotebookEdit":
      return [{ path, before: "", after: str(input.new_source) }];
  }
  return null;
}

export function editStats(tool: ToolItem): { added: number; deleted: number } | null {
  const edits = toolEdit(tool);
  if (!edits) return null;
  let added = 0;
  let deleted = 0;
  for (const edit of edits) {
    const stats = diffStats(diffLines(edit.before, edit.after));
    added += stats.added;
    deleted += stats.deleted;
  }
  return { added, deleted };
}

export interface Todo {
  content: string;
  status: "pending" | "in_progress" | "completed";
  activeForm?: string;
}

export function toolTodos(tool: ToolItem): Todo[] | null {
  if (tool.name !== "TodoWrite" || !tool.input || !Array.isArray(tool.input.todos)) return null;
  return (tool.input.todos as Record<string, unknown>[]).map((todo) => ({
    content: str(todo.content),
    status: (["pending", "in_progress", "completed"].includes(str(todo.status)) ? str(todo.status) : "pending") as Todo["status"],
    activeForm: str(todo.activeForm) || undefined,
  }));
}

export function toolPlan(tool: ToolItem): string | null {
  if (tool.name !== "ExitPlanMode" || !tool.input) return null;
  return str(tool.input.plan) || null;
}

export function pendingPermissions(items: ThreadItem[]): PermissionItem[] {
  return items.filter((item): item is PermissionItem => item.kind === "permission" && item.status === "pending");
}
