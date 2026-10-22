import { useEffect, useRef, useState } from "react";
import { ChevronDown, Download, FolderOpen, GitBranch, PanelLeft, PanelRight, Pencil, SquareTerminal, Trash2 } from "lucide-react";
import type { Session } from "../types";
import { useI18n } from "../i18n";
import { IconButton, MenuButton } from "./Sidebar";

interface Props {
  session: Session | null;
  sidebarHidden: boolean;
  panel: PanelTab | null;
  onShowSidebar: () => void;
  onPanel: (tab: PanelTab) => void;
  onRename: () => void;
  onDelete: () => void;
  onExport: (format: "md" | "html") => void;
  // The server offers a terminal (ccl web --shell).
  terminal: boolean;
}

// The title bar of claude.ai/code: the session title with its menu on the
// left, panel toggles on the right.
export type PanelTab = "overview" | "files" | "git" | "terminal";

export function SessionHeader({ session, sidebarHidden, panel, onShowSidebar, onPanel, onRename, onDelete, onExport, terminal }: Props) {
  const { t } = useI18n();
  const [menu, setMenu] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!menu) return;
    const close = (event: MouseEvent) => {
      if (ref.current && !ref.current.contains(event.target as Node)) setMenu(false);
    };
    document.addEventListener("mousedown", close);
    return () => document.removeEventListener("mousedown", close);
  }, [menu]);

  return (
    <header className="flex h-12 shrink-0 items-center gap-1 px-3">
      {/* On a phone the sidebar is an overlay, so its button is always here. */}
      <span className={sidebarHidden ? "" : "md:hidden"}>
        <IconButton title={t("toggleSidebar")} onClick={onShowSidebar}>
          <PanelLeft size={17} />
        </IconButton>
      </span>
      <div ref={ref} className="relative min-w-0">
        {session ? (
          <button
            onClick={() => setMenu(!menu)}
            className="flex min-w-0 items-center gap-1 rounded-md px-1 py-0.5 text-[13px] font-medium text-ink hover:bg-hover"
          >
            <span className="truncate">{session.title || t("untitled")}</span>
            <ChevronDown size={14} className="shrink-0 text-ink-3" />
          </button>
        ) : (
          <h1 className="px-1 text-[13px] font-medium text-ink-3">{t("newSession")}</h1>
        )}
        {menu && session && (
          <div className="absolute left-0 top-full z-30 mt-1 min-w-[160px] rounded-[10px] border border-line bg-surface p-1 shadow-lg">
            <MenuButton icon={<Pencil size={14} />} onClick={() => { setMenu(false); onRename(); }}>{t("rename")}</MenuButton>
            <MenuButton icon={<Download size={14} />} onClick={() => { setMenu(false); onExport("md"); }}>{t("exportMarkdown")}</MenuButton>
            <MenuButton icon={<Download size={14} />} onClick={() => { setMenu(false); onExport("html"); }}>{t("exportHTML")}</MenuButton>
            <MenuButton icon={<Trash2 size={14} />} danger onClick={() => { setMenu(false); onDelete(); }}>{t("delete")}</MenuButton>
          </div>
        )}
      </div>
      <span className="flex-1" />
      <IconButton title={t("files")} onClick={() => onPanel("files")} active={panel === "files"}>
        <FolderOpen size={16} />
      </IconButton>
      <IconButton title={t("git")} onClick={() => onPanel("git")} active={panel === "git"}>
        <GitBranch size={16} />
      </IconButton>
      {terminal && (
        <IconButton title={t("terminal")} onClick={() => onPanel("terminal")} active={panel === "terminal"}>
          <SquareTerminal size={16} />
        </IconButton>
      )}
      <IconButton title={t("togglePanel")} onClick={() => onPanel("overview")} active={panel === "overview"}>
        <PanelRight size={17} />
      </IconButton>
    </header>
  );
}
