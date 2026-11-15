import type { ResultPayload, Session, Usage } from "./types";
import type { Lang, Translate } from "./i18n";

const UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 86_400],
  ["month", 30 * 86_400],
  ["day", 86_400],
  ["hour", 3_600],
  ["minute", 60],
];

export function relativeTime(iso: string | number, lang: Lang, now = Date.now()): string {
  const time = typeof iso === "number" ? iso : new Date(iso).getTime();
  if (!time) return "";
  const seconds = Math.round((time - now) / 1000);
  const format = new Intl.RelativeTimeFormat(lang, { numeric: "auto" });
  for (const [unit, size] of UNITS) {
    if (Math.abs(seconds) >= size) return format.format(Math.round(seconds / size), unit);
  }
  return format.format(0, "second");
}

export function clockTime(ts: number, lang: Lang): string {
  if (!ts) return "";
  return new Date(ts).toLocaleTimeString(lang, { hour: "2-digit", minute: "2-digit" });
}

export function dayLabel(ts: number, lang: Lang, now = Date.now()): string {
  if (!ts) return "";
  const start = (value: number) => new Date(new Date(value).toDateString()).getTime();
  const days = Math.round((start(ts) - start(now)) / 86_400_000);
  if (days >= -1) return new Intl.RelativeTimeFormat(lang, { numeric: "auto" }).format(days, "day");
  return new Date(ts).toLocaleDateString(lang, { year: "numeric", month: "short", day: "numeric" });
}

export function money(value: number): string {
  return "$" + value.toFixed(4);
}

export function tokens(usage: Partial<Usage>): string {
  const input = usage.inputTokens || 0;
  const output = usage.outputTokens || 0;
  if (!input && !output) return "";
  return `${input.toLocaleString()} in / ${output.toLocaleString()} out`;
}

export type TurnState = "done" | "stopped" | "failed";

export function turnState(result: ResultPayload): TurnState {
  if (result.cancelled) return "stopped";
  if (result.isError) return "failed";
  return "done";
}

// turnSummary describes how a turn ended. usage.costUsd is Claude Code's
// running total for the session, never this turn's spend: that is turnUsage.
export function turnSummary(result: ResultPayload, t: Translate, model?: string): string[] {
  const turn = result.turnUsage;
  const usage = turn || result.usage || ({} as Usage);
  const parts: string[] = [t(turnState(result))];
  if (result.message) parts.push(result.message);
  if (model) parts.push(model);
  const counts = tokens(usage);
  if (counts) parts.push(counts);
  if (usage.durationMs) parts.push(`${(usage.durationMs / 1000).toFixed(1)}s`);
  if (!result.subscription) {
    if (turn && turn.costUsd) parts.push(t("thisTurn", { cost: money(turn.costUsd) }));
    if (result.usage && result.usage.costUsd) parts.push(t("total", { cost: money(result.usage.costUsd) }));
  }
  return parts;
}

// elapsed reads like Claude Code's task list: 8s, 9m 2s, 1h 3m.
export function elapsed(ms: number): string {
  const seconds = Math.max(0, Math.floor(ms / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ${seconds % 60}s`;
  return `${Math.floor(minutes / 60)}h ${minutes % 60}m`;
}

// tokenCount shortens a token count: 950, 12.3k, 1.2M.
export function tokenCount(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

export function fileSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}

export function basename(path: string): string {
  const trimmed = path.replace(/[\\/]+$/, "");
  const index = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  return index >= 0 ? trimmed.slice(index + 1) || trimmed : trimmed;
}

export function shortPath(path: string, keep = 2): string {
  const parts = path.split(/[\\/]/).filter(Boolean);
  if (parts.length <= keep) return path;
  return "…/" + parts.slice(-keep).join("/");
}

export type GroupLabel = "pinned" | "today" | "yesterday" | "previous7Days" | "older";

// groupSessions puts pinned conversations first, then the rest by the day
// they were last used.
export function groupSessions(sessions: Session[], now = new Date()): [GroupLabel, Session[]][] {
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate()).getTime();
  const day = 86_400_000;
  const groups: Record<GroupLabel, Session[]> = { pinned: [], today: [], yesterday: [], previous7Days: [], older: [] };
  for (const session of sessions) {
    const time = new Date(session.updatedAt).getTime();
    const label: GroupLabel = session.pinned ? "pinned" : time >= midnight ? "today" : time >= midnight - day ? "yesterday" : time >= midnight - 7 * day ? "previous7Days" : "older";
    groups[label].push(session);
  }
  return (Object.keys(groups) as GroupLabel[]).filter((label) => groups[label].length).map((label) => [label, groups[label]]);
}
