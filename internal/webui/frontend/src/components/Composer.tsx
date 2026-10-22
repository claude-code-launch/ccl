import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { AlarmClock, ArrowUp, AtSign, Blocks, ChevronDown, Clock, Cloud, FileText, FolderGit2, Gauge, GitBranch, LoaderCircle, Mic, MicOff, Paperclip, Plus, ShieldCheck, Square, SquareSlash, X } from "lucide-react";
import type { Attachment, Capabilities, ProviderView, Schedule, Session, Workspace } from "../types";
import { EFFORT_LEVELS, PERMISSION_MODES } from "../types";
import { basename, fileSize } from "../format";
import { useI18n } from "../i18n";

interface Pending {
  key: string;
  name: string;
  size: number;
  preview?: string;
  status: "uploading" | "ready" | "error";
  attachment?: Attachment;
  error?: string;
}

interface Props {
  running: boolean;
  workspace: string;
  // A new conversation picks its folder; a started one keeps its own.
  workspaces: Workspace[];
  workspaceId: string;
  onWorkspace: (id: string) => void;
  onOpenFolder?: () => void;
  // A new conversation in a git repository may run on a branch of its own.
  worktree?: { on: boolean; onToggle: () => void };
  session: Session | null;
  providers: ProviderView[];
  capabilities: Capabilities;
  onRoute: (patch: Partial<Pick<Session, "provider" | "model" | "effort" | "permissionMode">>) => void;
  onUpload: (files: File[]) => Promise<Attachment[]>;
  onSend: (text: string, attachments: string[]) => Promise<boolean>;
  onStop: () => void;
  // The page hands dropped files and @-mentions to the composer through these.
  registerAddFiles: (add: (files: File[]) => void) => void;
  registerMention: (mention: (path: string) => void) => void;
  onSearchFiles: (query: string) => Promise<string[]>;
  // Messages written while a turn runs; they go out when it ends.
  queued: { key: string; text: string; files: number }[];
  onRemoveQueued: (key: string) => void;
  // Tokens the last model call read, against the model's window.
  context?: { tokens: number; window: number };
  // Claude is waiting for an answer to a permission card.
  awaitingPermission: boolean;
  scheduled: Schedule[];
  onSchedule: (text: string, at: string) => Promise<boolean>;
  onUnschedule: (id: string) => void;
  onManageConnectors: () => void;
}

// The Web Speech API, where the browser has it (Chrome sends audio to its
// recognition service).
type Recognition = { lang: string; interimResults: boolean; continuous: boolean; start(): void; stop(): void; onresult: ((event: { results: ArrayLike<ArrayLike<{ transcript: string }> & { isFinal: boolean }> ; resultIndex: number }) => void) | null; onend: (() => void) | null };
const SpeechRecognitionImpl: (new () => Recognition) | undefined =
  typeof window !== "undefined" ? ((window as unknown as Record<string, unknown>).SpeechRecognition || (window as unknown as Record<string, unknown>).webkitSpeechRecognition) as (new () => Recognition) | undefined : undefined;

function inAnHour(): string {
  const date = new Date(Date.now() + 3_600_000);
  date.setSeconds(0, 0);
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

const isMac = typeof navigator !== "undefined" && /Mac|iPhone|iPad/.test(navigator.platform);

export function Composer(props: Props) {
  const { running, capabilities, onUpload, onSend, onStop, registerAddFiles } = props;
  const { t } = useI18n();
  const [text, setText] = useState("");
  const [pending, setPending] = useState<Pending[]>([]);
  const [sending, setSending] = useState(false);
  const [menu, setMenu] = useState<null | "main" | "connectors" | "schedule">(null);
  const [scheduleAt, setScheduleAt] = useState(inAnHour);
  const [listening, setListening] = useState(false);
  const recognition = useRef<Recognition | null>(null);
  const { lang } = useI18n();
  const [pick, setPick] = useState(0);
  const [caret, setCaret] = useState(0);
  const [files, setFiles] = useState<string[]>([]);
  const input = useRef<HTMLTextAreaElement>(null);
  const picker = useRef<HTMLInputElement>(null);
  const box = useRef<HTMLDivElement>(null);

  // The box grows with its content up to 40% of the window.
  useLayoutEffect(() => {
    const node = input.current;
    if (!node) return;
    node.style.height = "auto";
    node.style.height = Math.min(node.scrollHeight, window.innerHeight * 0.4) + "px";
  }, [text]);

  const addFiles = useCallback(
    async (files: File[]) => {
      if (!files.length) return;
      const entries: Pending[] = files.map((file) => ({
        key: `${Date.now()}-${Math.random().toString(36).slice(2)}`,
        name: file.name || "pasted.png",
        size: file.size,
        preview: file.type.startsWith("image/") ? URL.createObjectURL(file) : undefined,
        status: "uploading",
      }));
      setPending((list) => [...list, ...entries]);
      // One request per file, so one bad file does not fail the others.
      await Promise.all(
        files.map(async (file, index) => {
          const key = entries[index].key;
          try {
            const [attachment] = await onUpload([file]);
            setPending((list) => list.map((item) => (item.key === key ? { ...item, status: "ready", attachment } : item)));
          } catch (error) {
            const message = error instanceof Error ? error.message : String(error);
            setPending((list) => list.map((item) => (item.key === key ? { ...item, status: "error", error: message } : item)));
          }
        }),
      );
    },
    [onUpload],
  );

  useEffect(() => registerAddFiles(addFiles), [registerAddFiles, addFiles]);
  const { registerMention } = props;
  useEffect(
    () =>
      registerMention((path) => {
        setText((current) => current + (current && !/\s$/.test(current) ? " " : "") + "@" + path + " ");
        requestAnimationFrame(() => {
          const node = input.current;
          if (!node) return;
          node.focus();
          node.setSelectionRange(node.value.length, node.value.length);
          setCaret(node.value.length);
        });
      }),
    [registerMention],
  );

  const removePending = (key: string) =>
    setPending((list) => {
      const item = list.find((entry) => entry.key === key);
      if (item?.preview) URL.revokeObjectURL(item.preview);
      return list.filter((entry) => entry.key !== key);
    });

  // ⌘U / Ctrl+U attaches files, as on claude.ai.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.metaKey || event.ctrlKey) && !event.shiftKey && !event.altKey && event.key.toLowerCase() === "u") {
        event.preventDefault();
        picker.current?.click();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // Clicking outside closes the menus.
  useEffect(() => {
    if (!menu) return;
    const onDown = (event: MouseEvent) => {
      if (box.current && !box.current.contains(event.target as Node)) setMenu(null);
    };
    document.addEventListener("mousedown", onDown);
    return () => document.removeEventListener("mousedown", onDown);
  }, [menu]);

  // A message that starts with "/" and has no space yet is a command name.
  const slashQuery = /^\/(\S*)$/.exec(text)?.[1];
  // "@name" right before the caret refers to a workspace file.
  const mention = slashQuery === undefined ? /(^|\s)@([^\s@]*)$/.exec(text.slice(0, caret)) : null;
  const mentionQuery = mention ? mention[2] : undefined;
  const { onSearchFiles } = props;
  useEffect(() => {
    if (mentionQuery === undefined) return;
    let stale = false;
    const timer = setTimeout(async () => {
      try {
        const found = await onSearchFiles(mentionQuery);
        if (!stale) setFiles(found.slice(0, 12));
      } catch {
        if (!stale) setFiles([]);
      }
    }, 120);
    return () => {
      stale = true;
      clearTimeout(timer);
    };
  }, [mentionQuery, onSearchFiles]);
  const commands = useMemo(() => {
    if (slashQuery === undefined) return [];
    const needle = slashQuery.toLowerCase();
    const all = capabilities.slashCommands;
    const starts = all.filter((name) => name.toLowerCase().startsWith(needle));
    const contains = all.filter((name) => !name.toLowerCase().startsWith(needle) && name.toLowerCase().includes(needle));
    return [...starts, ...contains].slice(0, 50);
  }, [slashQuery, capabilities.slashCommands]);
  const slashOpen = slashQuery !== undefined;
  const mentionOpen = mentionQuery !== undefined;
  const suggestions = slashOpen ? commands : mentionOpen ? files : [];
  useEffect(() => setPick(0), [slashQuery, mentionQuery]);

  const chooseCommand = (name: string) => {
    setText(`/${name} `);
    input.current?.focus();
  };

  const chooseFile = (path: string) => {
    const start = caret - (mentionQuery || "").length - 1;
    const next = text.slice(0, start) + "@" + path + " " + text.slice(caret);
    setText(next);
    const position = start + path.length + 2;
    setCaret(position);
    requestAnimationFrame(() => {
      input.current?.focus();
      input.current?.setSelectionRange(position, position);
    });
  };

  const choose = (value: string) => (slashOpen ? chooseCommand(value) : chooseFile(value));

  const uploading = pending.some((item) => item.status === "uploading");
  const ready = pending.filter((item) => item.status === "ready" && item.attachment);
  // While a turn runs, sending queues the message for when it ends.
  const canSend = !sending && !uploading && (text.trim() !== "" || ready.length > 0);

  const submit = async () => {
    if (!canSend) return;
    const value = text.trim();
    const ids = ready.map((item) => item.attachment!.id);
    setSending(true);
    setText("");
    const kept = pending;
    setPending([]);
    const ok = await onSend(value, ids);
    if (ok) {
      for (const item of kept) if (item.preview) URL.revokeObjectURL(item.preview);
    } else {
      setText(value);
      setPending(kept);
    }
    setSending(false);
    input.current?.focus();
  };

  const onKeyDown = (event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    // Enter that confirms an IME composition (Chinese, Japanese…) is not a send.
    if (event.nativeEvent.isComposing || event.keyCode === 229) return;
    if ((slashOpen || mentionOpen) && suggestions.length) {
      if (event.key === "ArrowDown" || event.key === "ArrowUp") {
        event.preventDefault();
        const step = event.key === "ArrowDown" ? 1 : -1;
        setPick((index) => (index + step + suggestions.length) % suggestions.length);
        return;
      }
      if (event.key === "Tab" || (event.key === "Enter" && !event.shiftKey)) {
        event.preventDefault();
        choose(suggestions[pick] || suggestions[0]);
        return;
      }
    }
    if (event.key === "Escape") {
      setMenu(null);
      if (slashOpen) setText("");
      return;
    }
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      submit();
    }
  };

  const onPaste = (event: React.ClipboardEvent<HTMLTextAreaElement>) => {
    const files = Array.from(event.clipboardData.files);
    if (files.length) {
      event.preventDefault();
      addFiles(files);
    }
  };

  const hasHistory = (props.session?.turns || 0) > 0;
  return (
    <footer className="pb-2.5">
      <div ref={box} className="relative mx-auto max-w-[691px] pl-6 pr-8 max-sm:px-3">
        {menu === "main" && (
          <Popover>
            <MenuItem icon={<Paperclip size={17} />} label={t("addFiles")} shortcut={isMac ? "⌘ U" : "Ctrl U"}
              onClick={() => { setMenu(null); picker.current?.click(); }} />
            <MenuItem icon={<SquareSlash size={17} />} label={t("slashCommands")}
              onClick={() => { setMenu(null); setText("/"); input.current?.focus(); }} />
            <MenuItem icon={<Blocks size={17} />} label={t("connectors")} onClick={() => setMenu("connectors")} />
            <MenuItem icon={<AlarmClock size={17} />} label={t("scheduleSend")} onClick={() => { setScheduleAt(inAnHour()); setMenu("schedule"); }} />
          </Popover>
        )}
        {menu === "schedule" && (
          <Popover wide>
            <div className="px-3 pb-1 pt-2 text-xs font-medium text-ink-3">{t("scheduleSend")}</div>
            <div className="flex flex-wrap items-center gap-2 px-3 py-2">
              <label className="text-[13px] text-ink-2">{t("scheduleAt")}</label>
              <input
                type="datetime-local"
                value={scheduleAt}
                onChange={(event) => setScheduleAt(event.target.value)}
                className="rounded-lg border border-line bg-surface px-2 py-1 text-[13px] text-ink outline-none [color-scheme:inherit]"
              />
              <button
                disabled={!text.trim() || !scheduleAt}
                onClick={async () => {
                  const ok = await props.onSchedule(text.trim(), new Date(scheduleAt).toISOString());
                  if (ok) {
                    setText("");
                    setMenu(null);
                  }
                }}
                className="rounded-lg bg-clay px-3 py-1 text-[13px] font-medium text-white hover:bg-clay-hover disabled:opacity-40"
              >
                {t("scheduleConfirm")}
              </button>
            </div>
            {!text.trim() && <div className="px-3 pb-2 text-xs text-ink-3">{t("scheduleNeedText")}</div>}
          </Popover>
        )}
        {menu === "connectors" && (
          <Popover wide>
            <div className="px-3 pb-1 pt-2 text-xs font-medium text-ink-3">{t("connectorsTitle")}</div>
            {capabilities.mcpServers.length === 0 && <div className="px-3 py-2 text-[13px] text-ink-2">{t("noConnectors")}</div>}
            {capabilities.mcpServers.map((server) => (
              <div key={server.name} className="flex items-center gap-2.5 rounded-lg px-3 py-1.5 text-[14px] text-ink">
                <span className={"size-2 rounded-full " + (server.status === "connected" ? "bg-emerald-500" : server.status === "failed" ? "bg-red-500" : "bg-amber-500")} />
                <span className="truncate">{server.name}</span>
                <span className="ml-auto text-xs text-ink-3">{server.status}</span>
              </div>
            ))}
            <button onClick={() => { setMenu(null); props.onManageConnectors(); }} className="mt-1 w-full border-t border-line px-3 pb-1.5 pt-2 text-left text-[13px] text-clay hover:underline">
              {t("manageConnectors")}
            </button>
          </Popover>
        )}
        {(slashOpen || mentionOpen) && !menu && (
          <Popover wide>
            <div className="px-3 pb-1 pt-2 text-xs font-medium text-ink-3">{slashOpen ? t("commandsTitle") : t("mentionTitle")}</div>
            {suggestions.length === 0 && <div className="px-3 py-2 text-[13px] text-ink-2">{slashOpen ? t("noCommands") : t("noFiles")}</div>}
            <div className="max-h-72 overflow-y-auto">
              {suggestions.map((value, index) => (
                <button
                  key={value}
                  type="button"
                  onMouseDown={(event) => event.preventDefault()}
                  onClick={() => choose(value)}
                  onMouseEnter={() => setPick(index)}
                  className={"flex w-full min-w-0 items-center gap-1.5 rounded-lg px-3 py-1.5 text-left text-[14px] text-ink " + (index === pick ? "bg-active" : "")}
                >
                  {slashOpen ? <span className="font-mono text-ink-3">/</span> : <AtSign size={13} className="shrink-0 text-ink-3" />}
                  <span className="truncate">{value}</span>
                </button>
              ))}
            </div>
          </Popover>
        )}

        {props.awaitingPermission && (
          <div className="mb-1.5 flex items-center gap-2 rounded-[10px] border border-clay/50 bg-clay/[0.06] px-3 py-1.5 text-[13px] text-ink">
            <ShieldCheck size={14} className="shrink-0 text-clay" />
            {t("permWaiting")}
          </div>
        )}
        {props.scheduled.length > 0 && (
          <div className="mb-1.5 space-y-1">
            {props.scheduled.map((item) => (
              <div key={item.id} className="flex items-center gap-2 rounded-[10px] border border-dashed border-line px-3 py-1.5 text-[13px] text-ink-2">
                <AlarmClock size={13} className="shrink-0 text-ink-3" />
                <span className="truncate">{item.text}</span>
                <span className="ml-auto shrink-0 text-xs text-ink-3">{t("scheduled", { time: new Date(item.at).toLocaleString(lang, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) })}</span>
                <button onClick={() => props.onUnschedule(item.id)} title={t("remove")} aria-label={t("remove")} className="shrink-0 rounded p-0.5 text-ink-3 hover:bg-hover hover:text-ink">
                  <X size={13} />
                </button>
              </div>
            ))}
          </div>
        )}
        {props.queued.length > 0 && (
          <div className="mb-1.5 space-y-1">
            {props.queued.map((item) => (
              <div key={item.key} className="flex items-center gap-2 rounded-[10px] border border-dashed border-line px-3 py-1.5 text-[13px] text-ink-2">
                <Clock size={13} className="shrink-0 text-ink-3" />
                <span className="truncate">{item.text || t("filesOnly", { n: item.files })}</span>
                {item.files > 0 && item.text && <span className="shrink-0 text-ink-3">+{item.files}</span>}
                <span className="ml-auto shrink-0 text-xs text-ink-3">{t("queuedNext")}</span>
                <button onClick={() => props.onRemoveQueued(item.key)} title={t("remove")} aria-label={t("remove")} className="shrink-0 rounded p-0.5 text-ink-3 hover:bg-hover hover:text-ink">
                  <X size={13} />
                </button>
              </div>
            ))}
          </div>
        )}
        <div className="rounded-[14px] border border-line bg-surface p-2 transition focus-within:border-line-strong">
          {pending.length > 0 && (
            <div className="flex flex-wrap gap-2 px-1 pb-2 pt-1">
              {pending.map((item) => (
                <PendingChip key={item.key} item={item} onRemove={() => removePending(item.key)} />
              ))}
            </div>
          )}
          <div className="flex items-end gap-1.5">
            <button
              type="button"
              onClick={() => setMenu(menu ? null : "main")}
              title={t("addFiles")}
              aria-label={t("addFiles")}
              aria-expanded={!!menu}
              className={"grid size-7 shrink-0 place-items-center rounded-lg text-ink-2 transition hover:bg-hover hover:text-ink " + (menu ? "bg-active text-ink" : "")}
            >
              <Plus size={17} className={"transition " + (menu ? "rotate-45" : "")} />
            </button>
            <input
              ref={picker}
              type="file"
              multiple
              hidden
              onChange={(event) => {
                addFiles(Array.from(event.target.files || []));
                event.target.value = "";
              }}
            />
            <textarea
              ref={input}
              rows={1}
              value={text}
              autoFocus
              onChange={(event) => {
                setText(event.target.value);
                setCaret(event.target.selectionStart);
              }}
              onSelect={(event) => setCaret(event.currentTarget.selectionStart)}
              onKeyDown={onKeyDown}
              onPaste={onPaste}
              placeholder={hasHistory ? t("placeholderCommand") : t("placeholder")}
              className="block max-h-[40vh] min-h-7 flex-1 resize-none bg-transparent px-1 py-1 text-[14px] leading-5 text-ink outline-none placeholder:text-ink-3"
            />
            {SpeechRecognitionImpl && !running && (
              <button
                type="button"
                title={listening ? t("voiceStop") : t("voiceInput")}
                aria-label={listening ? t("voiceStop") : t("voiceInput")}
                onClick={() => {
                  if (listening) {
                    recognition.current?.stop();
                    return;
                  }
                  const recognizer = new SpeechRecognitionImpl();
                  recognizer.lang = lang === "zh" ? "zh-CN" : "en-US";
                  recognizer.interimResults = false;
                  recognizer.continuous = true;
                  recognizer.onresult = (event) => {
                    let heard = "";
                    for (let i = event.resultIndex; i < event.results.length; i++) {
                      if (event.results[i].isFinal) heard += event.results[i][0].transcript;
                    }
                    if (heard) setText((current) => (current && !/\s$/.test(current) ? current + " " : current) + heard);
                  };
                  recognizer.onend = () => setListening(false);
                  recognition.current = recognizer;
                  recognizer.start();
                  setListening(true);
                }}
                className={"grid size-7 shrink-0 place-items-center rounded-lg transition " + (listening ? "bg-red-500/15 text-red-500" : "text-ink-3 hover:bg-hover hover:text-ink")}
              >
                {listening ? <MicOff size={15} /> : <Mic size={15} />}
              </button>
            )}
            {(canSend || sending || uploading) && (
              <button
                onClick={submit}
                disabled={!canSend}
                title={running ? t("queueSend") : t("send")}
                aria-label={running ? t("queueSend") : t("send")}
                className={"grid size-7 shrink-0 place-items-center rounded-lg text-white transition disabled:opacity-50 " + (running ? "bg-ink-3 hover:bg-ink-2" : "bg-clay hover:bg-clay-hover")}
              >
                {sending || uploading ? <LoaderCircle size={15} className="animate-spin" /> : running ? <Clock size={15} /> : <ArrowUp size={16} strokeWidth={2.4} />}
              </button>
            )}
            {running && (
              <button
                onClick={onStop}
                title={t("stop")}
                aria-label={t("stop")}
                className="grid size-7 shrink-0 place-items-center rounded-lg bg-ink text-canvas transition hover:opacity-85"
              >
                <Square size={11} fill="currentColor" />
              </button>
            )}
          </div>
        </div>
        {/* One line: chips shrink and truncate rather than wrap. */}
        <div className="flex min-w-0 flex-nowrap items-center gap-x-1 overflow-hidden px-2 pt-1.5 text-[13px] text-ink-2">
          <Route {...props} />
          {props.context && <ContextRing {...props.context} />}
        </div>
      </div>
    </footer>
  );
}

// ContextRing shows how full the context window was at the last model call.
function ContextRing({ tokens, window: size }: { tokens: number; window: number }) {
  const { t } = useI18n();
  const ratio = Math.min(1, tokens / size);
  const radius = 6.5;
  const length = 2 * Math.PI * radius;
  const color = ratio > 0.85 ? "#dc2626" : ratio > 0.6 ? "#d97706" : "#3b82f6";
  return (
    <span title={t("contextUsed", { used: tokens.toLocaleString(), total: size.toLocaleString(), percent: Math.round(ratio * 100) })} className="ml-1 grid size-6 shrink-0 place-items-center">
      <svg width="16" height="16" viewBox="0 0 16 16" aria-hidden="true">
        <circle cx="8" cy="8" r={radius} fill="none" stroke="currentColor" strokeOpacity="0.2" strokeWidth="2" />
        <circle cx="8" cy="8" r={radius} fill="none" stroke={color} strokeWidth="2" strokeDasharray={`${ratio * length} ${length}`} strokeLinecap="round" transform="rotate(-90 8 8)" />
      </svg>
    </span>
  );
}

function Popover({ children, wide }: { children: ReactNode; wide?: boolean }) {
  return (
    <div
      className={
        "absolute bottom-full left-6 z-20 mb-2 rounded-[14px] border border-line bg-surface p-1.5 shadow-[0_0.5rem_2rem_rgba(0,0,0,0.12)] max-sm:left-3 dark:shadow-[0_0.5rem_2rem_rgba(0,0,0,0.5)] " +
        (wide ? "w-[min(420px,calc(100%-3.5rem))]" : "min-w-[300px]")
      }
    >
      {children}
    </div>
  );
}

function MenuItem({ icon, label, shortcut, onClick }: { icon: ReactNode; label: string; shortcut?: string; onClick: () => void }) {
  return (
    <button type="button" onClick={onClick} className="flex w-full items-center gap-3 rounded-[10px] px-3 py-2 text-left text-[15px] text-ink hover:bg-active">
      <span className="text-ink-2">{icon}</span>
      <span className="flex-1">{label}</span>
      {shortcut && <span className="text-sm text-ink-3">{shortcut}</span>}
    </button>
  );
}

function PendingChip({ item, onRemove }: { item: Pending; onRemove: () => void }) {
  const { t } = useI18n();
  const failed = item.status === "error";
  return (
    <div
      title={failed ? `${t("uploadFailed")}: ${item.error}` : item.name}
      className={"group/chip relative flex h-14 items-center overflow-hidden rounded-[10px] border bg-canvas " + (failed ? "border-red-400/60" : "border-line")}
    >
      {item.preview ? (
        <img src={item.preview} alt={item.name} className="h-full w-14 object-cover" />
      ) : (
        <div className="flex max-w-[200px] items-center gap-2.5 px-3">
          <FileText size={18} className="shrink-0 text-ink-3" />
          <span className="flex min-w-0 flex-col leading-tight">
            <span className="truncate text-sm text-ink">{item.name}</span>
            <span className={"text-xs " + (failed ? "text-red-500" : "text-ink-3")}>{failed ? t("uploadFailed") : fileSize(item.size)}</span>
          </span>
        </div>
      )}
      {item.status === "uploading" && (
        <div className="absolute inset-0 grid place-items-center bg-canvas/60">
          <LoaderCircle size={18} className="animate-spin text-ink-2" />
        </div>
      )}
      <button
        type="button"
        onClick={onRemove}
        title={t("remove")}
        aria-label={t("remove")}
        className="absolute right-1 top-1 grid size-5 place-items-center rounded-full bg-ink/80 text-canvas opacity-0 transition group-hover/chip:opacity-100"
      >
        <X size={12} />
      </button>
    </div>
  );
}

// Route is the provider / model / effort a turn launches with. It applies at
// the next turn; the server refuses a change while one runs.
// OPEN_FOLDER is the folder menu's "open another folder" entry.
const OPEN_FOLDER = "\u0000open";

function Route({ session, providers, running, workspace, workspaces, workspaceId, onWorkspace, onOpenFolder, worktree, capabilities, onRoute }: Props) {
  const { t } = useI18n();
  const provider = providers.find((item) => session && item.name === session.provider);
  const choices = provider?.modelChoices || [];
  const model = session?.model || "";
  const knownModel = !model || choices.some((choice) => choice.value === model);
  const locked = !session || running;
  const title = running ? t("routeLocked") : t("routeHint");
  return (
    <>
      <Chip icon={<Cloud size={14} />} label={t("provider")} title={title} disabled={locked} value={session?.provider || "default"} onChange={(value) => onRoute({ provider: value, model: "" })}>
        <option value="default">default</option>
        {providers.map((item) => (
          <option key={item.name} value={item.name}>{item.name}</option>
        ))}
      </Chip>
      {session?.worktree ? (
        <span title={`${workspace} · ${session.worktree.branch}`} className="flex max-w-[10rem] shrink-0 items-center gap-1.5 px-2 py-1 max-sm:hidden">
          <GitBranch size={14} className="shrink-0 text-ink-3" />
          <span className="truncate">{session.worktree.branch}</span>
        </span>
      ) : session ? (
        <span title={workspace} className="flex max-w-[10rem] shrink-0 items-center gap-1.5 px-2 py-1 max-sm:hidden">
          <FolderGit2 size={14} className="shrink-0 text-ink-3" />
          <span className="truncate">{basename(workspace) || "."}</span>
        </span>
      ) : (
        <Chip
          compact
          icon={<FolderGit2 size={14} />}
          label={t("workspace")}
          title={workspace}
          disabled={false}
          value={workspaceId}
          onChange={(value) => (value === OPEN_FOLDER ? onOpenFolder?.() : onWorkspace(value))}
        >
          {workspaces.map((item) => (
            <option key={item.id} value={item.id}>{item.name}</option>
          ))}
          {onOpenFolder && <option value={OPEN_FOLDER}>{t("openFolder")}</option>}
        </Chip>
      )}
      {!session && worktree && (
        <button
          type="button"
          aria-pressed={worktree.on}
          onClick={worktree.onToggle}
          title={t("worktreeHint")}
          className={
            "flex shrink-0 items-center gap-1.5 rounded-md px-2 py-1 max-sm:px-1.5 " +
            (worktree.on ? "bg-clay/10 text-clay" : "hover:bg-hover hover:text-ink")
          }
        >
          <GitBranch size={14} className={worktree.on ? "" : "text-ink-3"} />
          <span className="max-sm:hidden">{t("worktreeToggle")}</span>
        </button>
      )}
      <Chip
        compact
        icon={<ShieldCheck size={14} />}
        label={t("permissionMode")}
        title={title}
        disabled={locked}
        value={session?.permissionMode || capabilities.defaultPermissionMode || "default"}
        onChange={(value) => onRoute({ permissionMode: value })}
      >
        {PERMISSION_MODES.map((mode) => (
          <option key={mode} value={mode}>{t(("mode" + mode[0].toUpperCase() + mode.slice(1)) as "modeDefault")}</option>
        ))}
      </Chip>
      <span className="flex-1" />
      <Chip label={t("model")} title={title} disabled={locked} value={model} onChange={(value) => onRoute({ model: value })}>
        <option value="">{t("auto")}</option>
        {choices.map((choice) => (
          <option key={choice.value} value={choice.value}>{choice.label}</option>
        ))}
        {!knownModel && <option value={model}>{model}</option>}
      </Chip>
      <Chip
        compact
        icon={<Gauge size={14} className="sm:hidden" />}
        label={t("effort")}
        title={title}
        disabled={locked}
        value={session?.effort || ""}
        onChange={(value) => onRoute({ effort: value })}
        muted
      >
        <option value="">{t("effortDefault")}</option>
        {EFFORT_LEVELS.map((level) => (
          <option key={level} value={level}>{level}</option>
        ))}
      </Chip>
    </>
  );
}

function Chip({ icon, label, title, value, disabled, muted, compact, onChange, children }: {
  icon?: ReactNode;
  label: string;
  title: string;
  value: string;
  disabled: boolean;
  muted?: boolean;
  // compact shows only the icon on a phone; the menu still opens from it.
  compact?: boolean;
  onChange: (value: string) => void;
  children: ReactNode;
}) {
  return (
    <label
      title={`${label} · ${title}`}
      className={
        "relative flex min-w-0 items-center gap-1.5 rounded-md pl-2 " +
        (compact ? "shrink-0 max-sm:px-1.5 max-sm:py-1 " : "") +
        (muted ? "text-ink-3 " : "") +
        (disabled ? "opacity-70" : "hover:bg-hover hover:text-ink")
      }
    >
      {icon && <span className="shrink-0 text-ink-3">{icon}</span>}
      <select
        aria-label={label}
        value={value}
        disabled={disabled}
        onChange={(event) => onChange(event.target.value)}
        className={
          "min-w-[3.5rem] max-w-[180px] cursor-pointer appearance-none truncate bg-transparent py-1 pr-5 outline-none [field-sizing:content] disabled:cursor-default [&>option]:bg-surface [&>option]:text-ink " +
          (compact ? "max-sm:absolute max-sm:inset-0 max-sm:min-w-0 max-sm:opacity-0" : "")
        }
      >
        {children}
      </select>
      <ChevronDown size={12} className={"pointer-events-none absolute right-1 text-ink-3 " + (compact ? "max-sm:hidden" : "")} />
    </label>
  );
}
