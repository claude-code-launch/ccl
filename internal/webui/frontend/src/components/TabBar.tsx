import { useEffect, useRef, useState } from "react";
import { Bot, FolderOpen, GitBranch, LoaderCircle, MessageSquarePlus, PanelRight, Plus, SquareTerminal, X } from "lucide-react";
import { useI18n } from "../i18n";
import { IconButton, MenuButton, SidebarIcon } from "./Sidebar";

// The side panel's tab. The terminal is not one: terminals open as tabs.
export type PanelTab = "overview" | "files" | "git" | "agents";

// One tab of the bar: an open conversation, one of its sub-agents, the
// new-conversation draft, or a terminal.
export interface TabItem {
  key: string;
  kind: "chat" | "agent" | "draft" | "terminal";
  // Tabs that share a group (a conversation and its sub-agents) sit together
  // over one underline, like a browser's tab group.
  group?: string;
  title: string;
  // A tooltip with more than the title (the terminal's folder).
  hint?: string;
  running?: boolean;
  active: boolean;
  closable: boolean;
}

interface Props {
  tabs: TabItem[];
  // What a phone shows instead of the tabs.
  title: string;
  onActivate: (key: string) => void;
  onClose: (key: string) => void;
  onNewSession: () => void;
  // Absent when the server offers no terminal.
  onNewTerminal?: () => void;
  sidebarHidden: boolean;
  onShowSidebar: () => void;
  panel: PanelTab | null;
  onPanel: (tab: PanelTab) => void;
  // Opens (or returns from) a terminal of the current folder.
  onTerminal?: () => void;
  terminalActive: boolean;
  // How many sub-agents are working now.
  agentsRunning: number;
}

// The top bar spans the conversation and the side panel: the open tabs on the
// left, the panel buttons on the right. A phone has room for the title only;
// conversations are switched in the sidebar there.
export function TabBar(props: Props) {
  const { t } = useI18n();
  const { panel, onPanel, agentsRunning } = props;
  const strip = useRef<HTMLDivElement>(null);
  const active = props.tabs.find((tab) => tab.active)?.key;

  // Keep the active tab in view when it changes.
  useEffect(() => {
    strip.current?.querySelector<HTMLElement>('[data-active="true"]')?.scrollIntoView({ block: "nearest", inline: "nearest" });
  }, [active]);

  return (
    <header className="flex h-14 shrink-0 items-center gap-1 border-b border-line pl-3 pr-2">
      {/* On a phone the sidebar is an overlay, so its button is always here. */}
      <span className={props.sidebarHidden ? "" : "md:hidden"}>
        <IconButton title={t("toggleSidebar")} onClick={props.onShowSidebar}>
          <SidebarIcon />
        </IconButton>
      </span>
      <h1 className="min-w-0 flex-1 truncate px-1 text-[13px] font-medium text-ink md:hidden">{props.title}</h1>
      <div ref={strip} role="tablist" className="flex min-w-0 flex-1 items-center gap-1 self-stretch overflow-x-auto [scrollbar-width:none] max-md:hidden [&::-webkit-scrollbar]:hidden">
        {groups(props.tabs).map((run) => {
          const items = run.map((tab) => <Tab key={tab.key} tab={tab} onActivate={() => props.onActivate(tab.key)} onClose={() => props.onClose(tab.key)} />);
          if (run.length === 1 || !run[0].group) return items;
          return (
            <div key={"g:" + run[0].group} role="group" className="relative flex shrink-0 items-center gap-1 after:pointer-events-none after:absolute after:inset-x-1 after:-bottom-[5px] after:h-[2px] after:rounded-full after:bg-clay/80">
              {items}
            </div>
          );
        })}
        <NewTab onNewSession={props.onNewSession} onNewTerminal={props.onNewTerminal} />
      </div>
      <span className="ml-1 flex shrink-0 items-center gap-0.5">
        <IconButton title={t("files")} onClick={() => onPanel("files")} active={panel === "files"}>
          <FolderOpen size={16} />
        </IconButton>
        <IconButton title={t("git")} onClick={() => onPanel("git")} active={panel === "git"}>
          <GitBranch size={16} />
        </IconButton>
        <IconButton title={t("agents")} onClick={() => onPanel("agents")} active={panel === "agents"}>
          <span className="relative">
            <Bot size={16} />
            {agentsRunning > 0 && (
              <span className="absolute -right-2 -top-1.5 grid h-[14px] min-w-[14px] place-items-center rounded-full bg-clay px-[3px] text-[9.5px] font-medium leading-none text-white">{agentsRunning}</span>
            )}
          </span>
        </IconButton>
        {props.onTerminal && (
          <IconButton title={t("terminal")} onClick={props.onTerminal} active={props.terminalActive}>
            <SquareTerminal size={16} />
          </IconButton>
        )}
        <IconButton title={t("togglePanel")} onClick={() => onPanel("overview")} active={panel === "overview"}>
          <PanelRight size={17} />
        </IconButton>
      </span>
    </header>
  );
}

// groups splits the tabs into runs: neighbours of one group together, every
// other tab on its own.
function groups(tabs: TabItem[]): TabItem[][] {
  const out: TabItem[][] = [];
  for (const tab of tabs) {
    const last = out[out.length - 1];
    if (last && tab.group && last[0].group === tab.group) last.push(tab);
    else out.push([tab]);
  }
  return out;
}

function Tab({ tab, onActivate, onClose }: { tab: TabItem; onActivate: () => void; onClose: () => void }) {
  const { t } = useI18n();
  return (
    <div
      role="tab"
      aria-selected={tab.active}
      data-active={tab.active}
      tabIndex={0}
      title={tab.hint ? `${tab.title}\n${tab.hint}` : tab.title}
      onClick={onActivate}
      onKeyDown={(event) => (event.key === "Enter" || event.key === " ") && onActivate()}
      // A middle click closes, as in a browser.
      onAuxClick={(event) => event.button === 1 && tab.closable && onClose()}
      onMouseDown={(event) => event.button === 1 && event.preventDefault()}
      className={
        "group/tab flex h-8 min-w-[88px] max-w-[200px] shrink-0 cursor-pointer select-none items-center gap-1.5 rounded-lg pl-2.5 pr-1 text-[13px] transition-colors " +
        (tab.active ? "bg-active text-ink" : "text-ink-2 hover:bg-hover hover:text-ink")
      }
    >
      {tab.kind === "terminal" && <SquareTerminal size={14} className="shrink-0 text-ink-3" />}
      {tab.kind === "agent" && <Bot size={14} className="shrink-0 text-ink-3" />}
      <span className={"min-w-0 flex-1 truncate " + (tab.kind === "draft" && !tab.active ? "text-ink-3" : "")}>{tab.title}</span>
      {tab.running && <LoaderCircle size={13} className="shrink-0 animate-spin text-clay" aria-label={t("running")} />}
      {tab.closable && (
        <button
          type="button"
          tabIndex={-1}
          title={t("closeTab")}
          aria-label={t("closeTab")}
          onClick={(event) => {
            event.stopPropagation();
            onClose();
          }}
          className={"grid size-5 shrink-0 place-items-center rounded-md text-ink-3 hover:bg-active hover:text-ink " + (tab.active ? "" : "invisible group-hover/tab:visible")}
        >
          <X size={13} />
        </button>
      )}
    </div>
  );
}

// NewTab starts a conversation, or offers a terminal too when the server has one.
function NewTab({ onNewSession, onNewTerminal }: { onNewSession: () => void; onNewTerminal?: () => void }) {
  const { t } = useI18n();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const close = (event: MouseEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [open]);

  return (
    <div ref={ref} className="shrink-0">
      <IconButton title={onNewTerminal ? t("newTab") : t("newSession")} onClick={() => (onNewTerminal ? setOpen(!open) : onNewSession())} active={open}>
        <Plus size={16} />
      </IconButton>
      {/* Fixed: the tab strip scrolls sideways and would clip an absolute menu. */}
      {open && onNewTerminal && (
        <div
          className="fixed z-30 mt-1 min-w-[160px] rounded-[10px] border border-line bg-surface p-1 shadow-lg"
          style={{ left: ref.current?.getBoundingClientRect().left, top: ref.current?.getBoundingClientRect().bottom }}
        >
          <MenuButton icon={<MessageSquarePlus size={14} />} onClick={() => { setOpen(false); onNewSession(); }}>{t("newSession")}</MenuButton>
          <MenuButton icon={<SquareTerminal size={14} />} onClick={() => { setOpen(false); onNewTerminal(); }}>{t("terminalNew")}</MenuButton>
        </div>
      )}
    </div>
  );
}
