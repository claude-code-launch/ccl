import { useCallback, useLayoutEffect, useRef, useState } from "react";
import { ArrowDown, Check, ChevronRight, Copy, FileText, LoaderCircle, Volume2, VolumeX } from "lucide-react";
import type { ThreadItem } from "../thread";
import { toolActive } from "../thread";
import type { Attachment } from "../types";
import { clockTime, dayLabel, fileSize, turnState, turnSummary } from "../format";
import { useI18n } from "../i18n";
import { Markdown } from "./Markdown";
import { ToolCard } from "./ToolCard";
import { PermissionCard } from "./PermissionCard";
import type { PermissionDecision } from "../types";

interface Props {
  items: ThreadItem[];
  running: boolean;
  loading: boolean;
  workspace: string;
  route: string;
  attachmentURL: (id: string) => string;
  onPermission: (decision: PermissionDecision) => Promise<void>;
  // Set when no provider is configured yet: the empty page offers settings.
  onOnboard?: () => void;
}

// A long conversation renders its recent part; earlier items load on request.
const WINDOW = 150;

export function Thread({ items, running, loading, workspace, route, attachmentURL, onPermission, onOnboard }: Props) {
  const { t } = useI18n();
  const [shown, setShown] = useState(WINDOW);
  const scroller = useRef<HTMLDivElement>(null);
  // Follow the stream only while the reader is at the bottom.
  const stick = useRef(true);
  const [away, setAway] = useState(false);

  const onScroll = () => {
    const node = scroller.current;
    if (!node) return;
    const near = node.scrollHeight - node.scrollTop - node.clientHeight < 80;
    stick.current = near;
    setAway(!near);
  };

  useLayoutEffect(() => {
    const node = scroller.current;
    if (node && stick.current) node.scrollTop = node.scrollHeight;
  }, [items, running]);

  const toBottom = () => {
    const node = scroller.current;
    if (!node) return;
    stick.current = true;
    node.scrollTo({ top: node.scrollHeight, behavior: "smooth" });
  };

  const last = items[items.length - 1];
  const hidden = Math.max(0, items.length - shown);
  const visible = hidden ? items.slice(hidden) : items;
  const renderItems = useCallback(
    (children: ThreadItem[]) => children.map((child) => <Item key={child.key} item={child} streaming={false} attachmentURL={attachmentURL} renderItems={renderItems} onPermission={onPermission} />),
    [attachmentURL, onPermission],
  );
  const waiting =
    running &&
    (!last || (last.kind !== "text" && !(last.kind === "tool" && toolActive(last)) && !(last.kind === "permission" && last.status === "pending")));

  return (
    <div className="relative flex min-h-0 flex-1">
      {/* The thread never scrolls sideways: long words break, and code
          blocks and tables scroll on their own. */}
      <div ref={scroller} onScroll={onScroll} className="flex-1 overflow-y-auto overflow-x-hidden pb-8 pt-4">
        <div className="mx-auto flex w-full min-w-0 max-w-[691px] flex-col gap-1.5 pl-6 pr-8 [overflow-wrap:anywhere] max-sm:px-4">
          {!items.length && !loading && <Empty workspace={workspace} route={route} onOnboard={onOnboard} />}
          {hidden > 0 && (
            <button
              onClick={() => {
                // Keep the reader's place while older items are added above.
                const node = scroller.current;
                const before = node ? node.scrollHeight - node.scrollTop : 0;
                setShown((value) => value + WINDOW);
                requestAnimationFrame(() => {
                  if (node) node.scrollTop = node.scrollHeight - before;
                });
              }}
              className="mx-auto my-2 rounded-full border border-line px-3 py-1 text-[13px] text-ink-2 hover:bg-hover"
            >
              {t("showEarlier", { n: hidden })}
            </button>
          )}
          {visible.map((item) => (
            <Item key={item.key} item={item} streaming={running && item === last} attachmentURL={attachmentURL} renderItems={renderItems} onPermission={onPermission} />
          ))}
          {waiting && (
            <div className="flex items-center gap-2 py-1.5 pl-1 text-[14px] text-ink-3">
              <LoaderCircle size={14} className="animate-spin text-clay" />
              {t("working")}
            </div>
          )}
        </div>
      </div>
      {away && (
        <button
          onClick={toBottom}
          title={t("scrollToLatest")}
          aria-label={t("scrollToLatest")}
          className="absolute bottom-3 left-1/2 grid size-8 -translate-x-1/2 place-items-center rounded-full border border-line bg-surface text-ink-2 shadow-sm transition hover:text-ink"
        >
          <ArrowDown size={16} />
        </button>
      )}
    </div>
  );
}

function Empty({ workspace, route, onOnboard }: { workspace: string; route: string; onOnboard?: () => void }) {
  const { t } = useI18n();
  if (onOnboard) {
    return (
      <div className="m-auto flex max-w-md flex-col items-center pt-[22vh] text-center">
        <h2 className="font-display mb-3 text-[28px] font-normal tracking-tight text-ink">{t("onboardTitle")}</h2>
        <p className="mb-5 text-[14px] text-ink-2">{t("onboardText")}</p>
        <button onClick={onOnboard} className="rounded-lg bg-clay px-4 py-2 text-[14px] font-medium text-white hover:bg-clay-hover">
          {t("onboardAction")}
        </button>
      </div>
    );
  }
  return (
    <div className="m-auto flex max-w-lg flex-col items-center pt-[24vh] text-center">
      <h2 className="font-display mb-3 text-[30px] font-normal tracking-tight text-ink">{t("emptyTitle")}</h2>
      <p className="text-[13px] text-ink-3">
        {t("emptyIn", { path: "" })}
        <code className="rounded-md bg-hover px-1.5 py-0.5 font-mono text-[12.5px] text-ink-2">{workspace || "."}</code>
        {route && <> · {t("emptyWith", { route })}</>}
      </p>
    </div>
  );
}

function Attachments({ attachments, url }: { attachments: Attachment[]; url: (id: string) => string }) {
  if (!attachments.length) return null;
  return (
    <div className="flex flex-wrap justify-end gap-2">
      {attachments.map((attachment) =>
        attachment.image ? (
          <a key={attachment.id} href={url(attachment.id)} target="_blank" rel="noopener noreferrer" title={attachment.name}>
            <img
              src={url(attachment.id)}
              alt={attachment.name}
              className="h-24 max-w-[200px] rounded-[10px] border border-line object-cover"
            />
          </a>
        ) : (
          <a
            key={attachment.id}
            href={url(attachment.id)}
            title={attachment.name}
            className="flex max-w-[240px] items-center gap-2.5 rounded-[10px] border border-line px-3 py-2 text-left hover:bg-hover"
          >
            <FileText size={18} className="shrink-0 text-ink-3" />
            <span className="flex min-w-0 flex-col leading-tight">
              <span className="truncate text-sm text-ink">{attachment.name}</span>
              <span className="text-xs text-ink-3">{fileSize(attachment.size)}</span>
            </span>
          </a>
        ),
      )}
    </div>
  );
}

function CopyButton({ text }: { text: string }) {
  const { t } = useI18n();
  const [copied, setCopied] = useState(false);
  return (
    <button
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
        } catch {
          const area = document.createElement("textarea");
          area.value = text;
          document.body.appendChild(area);
          area.select();
          document.execCommand("copy");
          area.remove();
        }
        setCopied(true);
        setTimeout(() => setCopied(false), 1200);
      }}
      title={t("copy")}
      aria-label={t("copy")}
      className="grid size-6 place-items-center rounded-md text-ink-3 opacity-0 transition hover:bg-hover hover:text-ink group-hover/text:opacity-100"
    >
      {copied ? <Check size={13} /> : <Copy size={13} />}
    </button>
  );
}

// ReadButton reads a reply aloud with the browser's own speech synthesis.
function ReadButton({ text }: { text: string }) {
  const { t, lang } = useI18n();
  const [speaking, setSpeaking] = useState(false);
  if (typeof speechSynthesis === "undefined") return null;
  return (
    <button
      onClick={() => {
        if (speaking) {
          speechSynthesis.cancel();
          setSpeaking(false);
          return;
        }
        // Markup is not worth reading out.
        const plain = text.replace(/```[\s\S]*?```/g, " ").replace(/[`*_#>|]/g, "").replace(/\[([^\]]*)\]\([^)]*\)/g, "$1");
        const utterance = new SpeechSynthesisUtterance(plain);
        utterance.lang = lang === "zh" ? "zh-CN" : "en-US";
        utterance.onend = utterance.onerror = () => setSpeaking(false);
        speechSynthesis.cancel();
        speechSynthesis.speak(utterance);
        setSpeaking(true);
      }}
      title={speaking ? t("stopReading") : t("readAloud")}
      aria-label={speaking ? t("stopReading") : t("readAloud")}
      className={"grid size-6 place-items-center rounded-md text-ink-3 transition hover:bg-hover hover:text-ink " + (speaking ? "opacity-100" : "opacity-0 group-hover/text:opacity-100")}
    >
      {speaking ? <VolumeX size={13} /> : <Volume2 size={13} />}
    </button>
  );
}

function Thinking({ text, streaming }: { text: string; streaming: boolean }) {
  const { t } = useI18n();
  return (
    <details className="group/think my-0.5">
      <summary className="flex w-fit cursor-pointer list-none items-center gap-1.5 py-0.5 pl-1 text-[14px] text-ink-3 hover:text-ink-2 [&::-webkit-details-marker]:hidden">
        {streaming && <LoaderCircle size={13} className="animate-spin" />}
        <span>{streaming ? t("thinkingNow") : t("thinking")}</span>
        <span className="max-w-[420px] truncate text-[13px] opacity-70">{text.split("\n").find((line) => line.trim()) || ""}</span>
        <ChevronRight size={14} className="shrink-0 transition [details[open]>summary>&]:rotate-90" />
      </summary>
      <div className="mt-1 whitespace-pre-wrap border-l-2 border-line py-1 pl-3 text-[13.5px] italic leading-5 text-ink-3">{text}</div>
    </details>
  );
}

function Item({ item, streaming, attachmentURL, renderItems, onPermission }: {
  item: ThreadItem;
  streaming: boolean;
  attachmentURL: (id: string) => string;
  renderItems: (items: ThreadItem[]) => React.ReactNode;
  onPermission: (decision: PermissionDecision) => Promise<void>;
}) {
  const { t, lang } = useI18n();
  switch (item.kind) {
    case "day":
      return <div className="mx-auto mb-1 mt-4 text-xs text-ink-3">{dayLabel(item.ts, lang)}</div>;
    case "user":
      return (
        <div className="group/user mb-2 mt-6 flex flex-col items-end gap-1.5">
          <Attachments attachments={item.attachments} url={attachmentURL} />
          {item.text && (
            <div className="max-w-[85%] whitespace-pre-wrap break-words rounded-[10px] bg-bubble px-3 py-2 text-[14px] leading-[18px] text-ink">
              {item.text}
            </div>
          )}
          <span className="text-[11px] text-ink-3 opacity-0 transition group-hover/user:opacity-100">{clockTime(item.ts, lang)}</span>
        </div>
      );
    case "text":
      return (
        <div className={"group/text relative pl-1 " + (item.subagent ? "ml-5 border-l-2 border-line pl-4" : "")}>
          <Markdown text={item.text} streaming={streaming} />
          {!streaming && (
            <div className="absolute -right-7 top-0 flex flex-col max-sm:hidden">
              <CopyButton text={item.text} />
              <ReadButton text={item.text} />
            </div>
          )}
        </div>
      );
    case "thinking":
      return <Thinking text={item.text} streaming={streaming} />;
    case "tool":
      return <ToolCard tool={item} renderItems={renderItems} />;
    case "error":
      return (
        <div className="my-1 whitespace-pre-wrap rounded-xl border border-red-300/60 bg-red-50 px-3.5 py-2.5 font-mono text-[13px] text-red-700 dark:border-red-400/30 dark:bg-red-500/10 dark:text-red-300">
          {item.message}
        </div>
      );
    case "permission":
      return <PermissionCard item={item} onRespond={onPermission} />;
    case "queued":
      return <div className="px-1 py-1 text-sm italic text-ink-3">{t("queued")}</div>;
    case "turn-end": {
      const state = turnState(item.result);
      // An imported turn has nothing to report beyond having ended.
      const usage = item.result.turnUsage || item.result.usage;
      if (state === "done" && !item.result.message && !(usage && (usage.outputTokens || usage.durationMs || (usage.costUsd && !item.result.subscription)))) return null;
      const color = state === "stopped" ? "text-clay" : state === "failed" ? "text-red-600 dark:text-red-400" : "text-ink-3";
      return (
        <div className={"mb-1 mt-1 flex flex-wrap items-center gap-x-1.5 pl-1 text-xs " + color}>
          {turnSummary(item.result, t, item.model).map((part, index) => (
            <span key={index} className={index ? "text-ink-3" : ""}>
              {index ? "· " : ""}
              {part}
            </span>
          ))}
        </div>
      );
    }
  }
}
