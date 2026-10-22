import { createElement } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import type { ThreadItem } from "./thread";
import { toolEdit, toolPlan, toolTodos } from "./thread";
import type { Translate } from "./i18n";

// Transcript export: Markdown for pasting anywhere, HTML for a standalone,
// readable copy. Both are built in the browser from the rendered thread.

export function transcriptMarkdown(title: string, items: ThreadItem[], t: Translate): string {
  const out: string[] = [`# ${title}`, ""];
  let speaker = "";
  const walk = (list: ThreadItem[], depth: number) => {
    const quote = depth ? "> ".repeat(depth) : "";
    for (const item of list) {
      if (depth === 0 && item.kind !== "user" && item.kind !== "day" && item.kind !== "turn-end" && speaker !== "claude") {
        speaker = "claude";
        out.push("## Claude", "");
      }
      switch (item.kind) {
        case "user":
          speaker = "user";
          out.push(`## ${t("you")}`, "", item.text || "", ...item.attachments.map((a) => `- 📎 ${a.name}`), "");
          break;
        case "text":
          // A blank line first, so text after a tool list is not read as part of it.
          out.push("", ...item.text.split("\n").map((line) => quote + line), "");
          break;
        case "thinking":
          out.push(`<details><summary>${t("thinking")}</summary>`, "", item.text, "", "</details>", "");
          break;
        case "tool": {
          const todos = toolTodos(item);
          const plan = toolPlan(item);
          if (todos) {
            out.push(...todos.map((todo) => `${quote}- [${todo.status === "completed" ? "x" : " "}] ${todo.content}`), "");
          } else if (plan) {
            out.push(`${quote}**${t("plan")}**`, "", plan, "");
          } else {
            const line = item.description || item.summary;
            out.push(`${quote}- \`${item.name}\` ${line}`);
            const edits = toolEdit(item);
            if (edits) out.push(...edits.filter((edit) => edit.path && edit.path !== line).map((edit) => `${quote}  - ${edit.path}`));
            if (item.children.length) {
              out.push("");
              walk(item.children, depth + 1);
            }
          }
          break;
        }
        case "error":
          out.push("```", item.message, "```", "");
          break;
      }
    }
  };
  walk(items, 0);
  return out.join("\n").replace(/\n{3,}/g, "\n\n");
}

export async function transcriptHTML(title: string, markdown: string): Promise<string> {
  // Only the server renderer is loaded on demand; the rest is already in the page.
  const { renderToStaticMarkup } = await import("react-dom/server");
  const body = renderToStaticMarkup(createElement(ReactMarkdown, { remarkPlugins: [remarkGfm] }, markdown));
  const escaped = title.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!);
  return `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>${escaped}</title>
<style>
:root{color-scheme:light dark;--bg:#faf9f7;--ink:#1a1a18;--line:rgba(0,0,0,.1);--code:rgba(0,0,0,.04)}
@media (prefers-color-scheme:dark){:root{--bg:#151515;--ink:#f0efec;--line:rgba(255,255,255,.12);--code:rgba(255,255,255,.05)}}
body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,"PingFang SC",sans-serif}
main{max-width:760px;margin:0 auto;padding:32px 20px}
pre,code{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:13px}
pre{background:var(--code);border:1px solid var(--line);border-radius:10px;padding:12px;overflow:auto}
:not(pre)>code{background:var(--code);border-radius:5px;padding:1px 5px}
table{border-collapse:collapse}th,td{border:1px solid var(--line);padding:4px 10px}
blockquote{border-left:3px solid var(--line);margin:0;padding-left:12px;opacity:.85}
h2{margin-top:2em;border-top:1px solid var(--line);padding-top:1em}
</style></head><body><main>${body}</main></body></html>`;
}

export function download(name: string, content: string, type: string) {
  const url = URL.createObjectURL(new Blob([content], { type }));
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  document.body.appendChild(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export function safeFileName(title: string): string {
  return (title.replace(/[\\/:*?"<>|\s]+/g, "-").replace(/^-+|-+$/g, "") || "conversation").slice(0, 80);
}
