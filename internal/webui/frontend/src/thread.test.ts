import { describe, expect, it } from "vitest";
import { buildThread, latestCapabilities } from "./thread";
import { basename, dayLabel, fileSize, relativeTime, shortPath, turnSummary } from "./format";
import { translator } from "./i18n";
import type { StoredEvent } from "./types";

let seq = 0;
const ev = (type: string, payload: unknown = {}, ts = 1_700_000_000_000): StoredEvent => ({ seq: ++seq, type, payload, ts });

describe("buildThread", () => {
  it("joins streamed text and carries the turn's model to its summary", () => {
    const items = buildThread([
      ev("user", { text: "hi" }),
      ev("init", { sessionId: "s", model: "opus-x" }),
      ev("token", { text: "Hel" }),
      ev("token", { text: "lo" }),
      ev("result", { usage: { costUsd: 0.02 }, turnUsage: { costUsd: 0.01 }, isError: false }),
    ]);
    expect(items.map((item) => item.kind)).toEqual(["day", "user", "text", "turn-end"]);
    const text = items[2];
    expect(text.kind === "text" && text.text).toBe("Hello");
    const end = items[3];
    expect(end.kind === "turn-end" && end.model).toBe("opus-x");
  });

  it("keeps a user message's attachments", () => {
    const items = buildThread([
      ev("user", { text: "", attachments: [{ id: "a-1.png", name: "1.png", type: "image/png", size: 3, image: true }] }),
    ]);
    const user = items.find((item) => item.kind === "user");
    expect(user && user.kind === "user" && user.attachments.map((a) => a.name)).toEqual(["1.png"]);
  });

  it("merges the early and the complete announcement of one tool call", () => {
    const items = buildThread([
      ev("user", { text: "list" }),
      ev("tool_use", { id: "t1", name: "Bash", summary: "" }),
      ev("tool_use", { id: "t1", name: "Bash", summary: "ls -la" }),
      ev("tool_result", { id: "t1", ok: true, output: "a\nb" }),
    ]);
    const tools = items.filter((item) => item.kind === "tool");
    expect(tools).toHaveLength(1);
    expect(tools[0]).toMatchObject({ summary: "ls -la", status: "ok", output: "a\nb" });
  });

  it("scopes tool ids to their turn", () => {
    const items = buildThread([
      ev("user", { text: "one" }),
      ev("tool_use", { id: "same", name: "Read", summary: "a.go" }),
      ev("tool_result", { id: "same", ok: true, output: "" }),
      ev("result", { usage: {}, isError: false }),
      ev("user", { text: "two" }),
      ev("tool_use", { id: "same", name: "Read", summary: "b.go" }),
    ]);
    const tools = items.filter((item) => item.kind === "tool");
    expect(tools.map((tool) => tool.kind === "tool" && tool.summary)).toEqual(["a.go", "b.go"]);
  });

  it("marks a tool that never reported back as failed when the turn ends", () => {
    const items = buildThread([
      ev("user", { text: "x" }),
      ev("tool_use", { id: "t", name: "Bash", summary: "sleep 99" }),
      ev("result", { usage: {}, isError: false, cancelled: true }),
    ]);
    const tool = items.find((item) => item.kind === "tool");
    expect(tool && tool.kind === "tool" && tool.status).toBe("err");
  });

  it("shows the queued status", () => {
    const items = buildThread([ev("user", { text: "x" }), ev("status", { state: "queued" })]);
    expect(items.some((item) => item.kind === "queued")).toBe(true);
  });

  it("separates subagent text from the main reply", () => {
    const items = buildThread([
      ev("user", { text: "x" }),
      ev("token", { text: "main" }),
      ev("token", { text: "sub", subagent: true }),
    ]);
    const texts = items.filter((item) => item.kind === "text");
    expect(texts.map((item) => item.kind === "text" && item.subagent)).toEqual([false, true]);
  });
});

describe("latestCapabilities", () => {
  it("reads the newest init event", () => {
    expect(latestCapabilities([ev("user", { text: "x" })])).toBeNull();
    const caps = latestCapabilities([
      ev("init", { sessionId: "s", slashCommands: ["old"] }),
      ev("init", { sessionId: "s", slashCommands: ["review"], mcpServers: [{ name: "gh", status: "connected" }] }),
    ]);
    expect(caps).toEqual({ slashCommands: ["review"], mcpServers: [{ name: "gh", status: "connected" }] });
  });
});

describe("turnSummary", () => {
  const t = translator("en");
  it("reports this turn's cost apart from the running total", () => {
    const parts = turnSummary(
      {
        usage: { inputTokens: 120, outputTokens: 6, costUsd: 0.0375, durationMs: 900 },
        turnUsage: { inputTokens: 120, outputTokens: 6, costUsd: 0.0125, durationMs: 900 },
        isError: false,
      },
      t,
      "opus-x",
    );
    expect(parts).toEqual(["Done", "opus-x", "120 in / 6 out", "0.9s", "$0.0125 this turn", "$0.0375 total"]);
  });

  it("names a stopped turn in the page language", () => {
    expect(turnSummary({ usage: {} as never, isError: false, cancelled: true }, translator("zh"))[0]).toBe("已停止");
  });
});

describe("format helpers", () => {
  it("relativeTime", () => {
    const now = 1_700_000_000_000;
    expect(relativeTime(now - 10_000, "en", now)).toBe("now");
    expect(relativeTime(now - 3 * 60_000, "en", now)).toBe("3 minutes ago");
    expect(relativeTime(now - 5 * 86_400_000, "zh", now)).toBe("5天前");
  });

  it("dayLabel", () => {
    const now = new Date(2026, 9, 22, 15).getTime();
    expect(dayLabel(now - 3_600_000, "en", now)).toBe("today");
    expect(dayLabel(now - 86_400_000, "zh", now)).toBe("昨天");
  });

  it("sizes and paths", () => {
    expect(fileSize(2048)).toBe("2.0 KB");
    expect(basename("/Users/me/Space/ccl/")).toBe("ccl");
    expect(shortPath("/Users/me/Space/GitHub/ccl")).toBe("…/GitHub/ccl");
  });
});

import { diffLines, diffStats, withContext } from "./diff";
import { editStats, toolTodos } from "./thread";

describe("sub-agents and thinking", () => {
  it("nests a sub-agent's events under its Task call", () => {
    const items = buildThread([
      ev("user", { text: "x" }),
      ev("tool_use", { id: "task1", name: "Task", summary: "", description: "Explore the repo" }),
      ev("token", { text: "sub text", subagent: true, parent: "task1" }),
      ev("tool_use", { id: "r1", name: "Read", summary: "a.go", subagent: true, parent: "task1" }),
      ev("tool_result", { id: "r1", ok: true, output: "", parent: "task1" }),
      ev("tool_result", { id: "task1", ok: true, output: "done" }),
      ev("token", { text: "main text" }),
    ]);
    const root = items.map((item) => item.kind);
    expect(root).toEqual(["day", "user", "tool", "text"]);
    const task = items[2];
    if (task.kind !== "tool") throw new Error("not a tool");
    expect(task.children.map((child) => child.kind)).toEqual(["text", "tool"]);
    expect(task.status).toBe("ok");
  });

  it("collects thinking into its own block before the reply", () => {
    const items = buildThread([
      ev("user", { text: "x" }),
      ev("thinking", { text: "let me " }),
      ev("thinking", { text: "think" }),
      ev("token", { text: "answer" }),
    ]);
    const thinking = items.find((item) => item.kind === "thinking");
    expect(thinking && thinking.kind === "thinking" && thinking.text).toBe("let me think");
    expect(items[items.length - 1].kind).toBe("text");
  });

  it("reads edit stats and todos from tool input", () => {
    const items = buildThread([
      ev("user", { text: "x" }),
      ev("tool_use", { id: "e", name: "Edit", summary: "a.go", input: { file_path: "a.go", old_string: "a\nb\n", new_string: "a\nB\nc\n" } }),
      ev("tool_use", { id: "w", name: "TodoWrite", summary: "", input: { todos: [{ content: "Plan", status: "completed" }, { content: "Build", status: "in_progress" }] } }),
    ]);
    const [edit, todo] = items.filter((item) => item.kind === "tool");
    if (edit.kind !== "tool" || todo.kind !== "tool") throw new Error("not tools");
    expect(editStats(edit)).toEqual({ added: 2, deleted: 1 });
    expect(toolTodos(todo)?.map((t) => t.status)).toEqual(["completed", "in_progress"]);
  });
});

describe("diffLines", () => {
  it("finds the changed lines", () => {
    const lines = diffLines("a\nb\nc\n", "a\nx\nc\nd\n");
    expect(lines.map((l) => l.type[0] + l.text).join(" ")).toBe("sa db ax sc ad");
    expect(diffStats(lines)).toEqual({ added: 2, deleted: 1 });
  });

  it("folds long unchanged runs", () => {
    const before = Array.from({ length: 20 }, (_, i) => `l${i}`).join("\n");
    const after = before.replace("l10", "L10");
    const folded = withContext(diffLines(before, after), 2);
    expect(folded.filter((line) => line.type === "gap").length).toBe(2);
  });
});

describe("permissions", () => {
  it("tracks a request until it is answered or its turn ends", () => {
    const items = buildThread([
      ev("user", { text: "x" }),
      ev("permission", { requestId: "a", toolName: "Bash", summary: "rm -rf build", canRemember: true }),
      ev("permission", { requestId: "b", toolName: "Edit", summary: "a.go" }),
      ev("permission_resolved", { requestId: "a", behavior: "allow" }),
      ev("result", { usage: {}, isError: false }),
    ]);
    const [a, b] = items.filter((item) => item.kind === "permission");
    expect(a.kind === "permission" && a.status).toBe("allow");
    expect(b.kind === "permission" && b.status).toBe("cancelled");
  });
});
