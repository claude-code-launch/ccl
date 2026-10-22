import { memo, useRef, useState } from "react";
import type { ComponentPropsWithoutRef, ReactNode } from "react";
import ReactMarkdown from "react-markdown";
import remarkGfm from "remark-gfm";
import rehypeHighlight from "rehype-highlight";
import { Check, Copy } from "lucide-react";
import { useI18n } from "../i18n";

// Model output is rendered without raw HTML (react-markdown's default) and
// with its default URL filter, so a reply cannot inject markup or script URLs.

function CodeBlock({ children, ...props }: ComponentPropsWithoutRef<"pre">) {
  const { t } = useI18n();
  const ref = useRef<HTMLPreElement>(null);
  const [copied, setCopied] = useState(false);
  const language = languageOf(children);
  const copy = async () => {
    const text = ref.current?.innerText || "";
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // navigator.clipboard needs a secure context; a LAN http:// bind is not.
      const area = document.createElement("textarea");
      area.value = text;
      document.body.appendChild(area);
      area.select();
      document.execCommand("copy");
      area.remove();
    }
    setCopied(true);
    setTimeout(() => setCopied(false), 1200);
  };
  return (
    <div className="not-prose my-2.5 overflow-hidden rounded-[10px] border border-line bg-code">
      <div className="flex items-center justify-between border-b border-line px-3 py-1 font-mono text-xs text-ink-3">
        <span>{language || "text"}</span>
        <button
          type="button"
          onClick={copy}
          className="flex items-center gap-1 rounded-md px-2 py-0.5 hover:bg-hover hover:text-ink"
        >
          {copied ? <Check size={13} /> : <Copy size={13} />}
          {copied ? t("copied") : t("copy")}
        </button>
      </div>
      <pre ref={ref} {...props} className="overflow-x-auto px-3 py-2.5 font-mono text-[12.5px] leading-[1.5] text-ink">
        {children}
      </pre>
    </div>
  );
}

function languageOf(children: ReactNode): string {
  const child = Array.isArray(children) ? children[0] : children;
  if (child && typeof child === "object" && "props" in child) {
    const className = String((child.props as { className?: string }).className || "");
    const match = className.match(/language-([\w+#.-]+)/);
    if (match) return match[1];
  }
  return "";
}

export const Markdown = memo(function Markdown({ text, streaming }: { text: string; streaming?: boolean }) {
  return (
    <div
      className={
        "prose prose-sm max-w-none text-[14px] leading-5 text-ink prose-p:my-2 prose-p:leading-5 prose-li:my-0.5 prose-li:leading-5 prose-ul:my-2 prose-ol:my-2 prose-headings:mb-2 prose-headings:mt-4 prose-headings:font-semibold prose-headings:text-ink prose-strong:text-ink prose-a:text-clay " +
        "prose-blockquote:border-line-strong prose-blockquote:text-ink-2 prose-hr:border-line prose-th:text-ink " +
        "prose-code:before:content-none prose-code:after:content-none prose-code:rounded-md prose-code:bg-hover prose-code:px-1.5 prose-code:py-0.5 prose-code:font-normal prose-code:text-[0.86em] prose-code:text-ink " +
        "prose-table:text-sm prose-th:bg-code prose-th:px-3 prose-td:px-3 prose-table:border prose-table:border-line prose-tr:border-line prose-thead:border-line " +
        (streaming ? "streaming" : "")
      }
    >
      <ReactMarkdown
        remarkPlugins={[remarkGfm]}
        rehypePlugins={[[rehypeHighlight, { detect: false }]]}
        components={{
          pre: CodeBlock,
          a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" />,
        }}
      >
        {text}
      </ReactMarkdown>
    </div>
  );
});
