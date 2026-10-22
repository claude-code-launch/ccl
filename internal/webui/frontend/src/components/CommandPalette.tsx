import { useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { History, MessageSquare, Search } from "lucide-react";
import type { ClaudeSession, Session } from "../types";
import { relativeTime } from "../format";
import { useI18n } from "../i18n";

export interface PaletteAction {
  id: string;
  label: string;
  icon: ReactNode;
  shortcut?: string;
  run: () => void;
}

interface Props {
  actions: PaletteAction[];
  sessions: Session[];
  claudeSessions: ClaudeSession[];
  onSelect: (id: string) => void;
  onImport: (id: string) => void;
  onClose: () => void;
}

type Entry = { key: string; group: string; label: string; hint?: string; icon: ReactNode; run: () => void };

// ⌘K: jump to a session, import a Claude Code session, or run an action.
export function CommandPalette({ actions, sessions, claudeSessions, onSelect, onImport, onClose }: Props) {
  const { t, lang } = useI18n();
  const [query, setQuery] = useState("");
  const [index, setIndex] = useState(0);
  const list = useRef<HTMLDivElement>(null);

  const entries = useMemo(() => {
    const needle = query.trim().toLowerCase();
    const matches = (text: string) => !needle || text.toLowerCase().includes(needle);
    const out: Entry[] = [];
    for (const action of actions) {
      if (matches(action.label)) out.push({ key: "a" + action.id, group: t("actions"), label: action.label, hint: action.shortcut, icon: action.icon, run: action.run });
    }
    for (const session of sessions) {
      const label = session.title || t("untitled");
      if (matches(label)) {
        out.push({ key: "s" + session.id, group: t("recents"), label, hint: relativeTime(session.updatedAt, lang), icon: <MessageSquare size={15} />, run: () => onSelect(session.id) });
      }
    }
    for (const native of claudeSessions) {
      if (native.webSession || !matches(native.title)) continue;
      out.push({ key: "c" + native.id, group: t("claudeSessions"), label: native.title || native.id, hint: relativeTime(native.updated, lang), icon: <History size={15} />, run: () => onImport(native.id) });
    }
    return out.slice(0, 80);
  }, [query, actions, sessions, claudeSessions, t, lang, onSelect, onImport]);

  useEffect(() => setIndex(0), [query]);
  useEffect(() => {
    list.current?.querySelector(`[data-index="${index}"]`)?.scrollIntoView({ block: "nearest" });
  }, [index]);

  const run = (entry?: Entry) => {
    if (!entry) return;
    onClose();
    entry.run();
  };

  let lastGroup = "";
  return (
    <div className="fixed inset-0 z-40 flex items-start justify-center bg-black/40 px-4 pt-[14vh]" onMouseDown={onClose}>
      <div className="w-full max-w-[560px] overflow-hidden rounded-[14px] border border-line bg-surface shadow-2xl" onMouseDown={(event) => event.stopPropagation()}>
        <label className="flex items-center gap-2.5 border-b border-line px-4">
          <Search size={16} className="text-ink-3" />
          <input
            autoFocus
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            onKeyDown={(event) => {
              if (event.nativeEvent.isComposing) return;
              if (event.key === "ArrowDown" || event.key === "ArrowUp") {
                event.preventDefault();
                const step = event.key === "ArrowDown" ? 1 : -1;
                setIndex((value) => (value + step + entries.length) % Math.max(entries.length, 1));
              } else if (event.key === "Enter") {
                event.preventDefault();
                run(entries[index]);
              } else if (event.key === "Escape") {
                onClose();
              }
            }}
            placeholder={t("paletteHint")}
            className="h-12 flex-1 bg-transparent text-[15px] text-ink outline-none placeholder:text-ink-3"
          />
        </label>
        <div ref={list} className="max-h-[50vh] overflow-y-auto p-1.5">
          {!entries.length && <div className="px-3 py-6 text-center text-[13px] text-ink-3">{t("noMatches")}</div>}
          {entries.map((entry, position) => {
            const heading = entry.group !== lastGroup ? entry.group : "";
            lastGroup = entry.group;
            return (
              <div key={entry.key}>
                {heading && <div className="px-3 pb-1 pt-2 text-xs text-ink-3">{heading}</div>}
                <button
                  data-index={position}
                  onClick={() => run(entry)}
                  onMouseEnter={() => setIndex(position)}
                  className={"flex w-full items-center gap-3 rounded-lg px-3 py-2 text-left text-[14px] text-ink " + (position === index ? "bg-active" : "")}
                >
                  <span className="text-ink-3">{entry.icon}</span>
                  <span className="min-w-0 flex-1 truncate">{entry.label}</span>
                  {entry.hint && <span className="shrink-0 text-xs text-ink-3">{entry.hint}</span>}
                </button>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
