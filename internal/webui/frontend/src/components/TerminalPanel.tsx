import { useEffect, useRef, useState } from "react";
import { Terminal } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { Plus, X } from "lucide-react";
import type { Api } from "../api";
import { useI18n } from "../i18n";
import { IconButton } from "./Sidebar";

// The web terminal (ccl web --shell). It is its own chunk. Closing the panel
// leaves the shell running; the panel re-attaches and the server replays the
// recent output. Only the close button ends the shell.

interface Props {
  api: Api;
  id: string | null;
  onId: (id: string | null) => void;
  onError: (error: unknown) => void;
}

function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export default function TerminalPanel({ api, id, onId, onError }: Props) {
  const { t } = useI18n();
  const host = useRef<HTMLDivElement>(null);
  const [exited, setExited] = useState(false);

  useEffect(() => {
    if (!host.current) return;
    const term = new Terminal({
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
      fontSize: 12.5,
      cursorBlink: true,
      allowProposedApi: false,
      theme: {
        background: cssVar("--canvas"),
        foreground: cssVar("--ink"),
        cursor: cssVar("--clay"),
        selectionBackground: "rgba(59,130,246,0.3)",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host.current);
    fit.fit();

    let closed = false;
    let source: EventSource | null = null;
    let current = id;
    let pending = "";
    let timer: ReturnType<typeof setTimeout> | undefined;

    // Keystrokes go out in small batches rather than one request each.
    const flush = () => {
      timer = undefined;
      if (!current || !pending) return;
      const data = pending;
      pending = "";
      api.terminalInput(current, data).catch(() => setExited(true));
    };
    const input = term.onData((data) => {
      pending += data;
      if (!timer) timer = setTimeout(flush, 10);
    });

    const attach = (terminalId: string) => {
      source = new EventSource(api.terminalStreamURL(terminalId, 0));
      source.addEventListener("output", (message) => {
        const { data } = JSON.parse((message as MessageEvent<string>).data) as { data: string };
        const bytes = Uint8Array.from(atob(data), (c) => c.charCodeAt(0));
        term.write(bytes);
      });
      source.addEventListener("exit", () => {
        setExited(true);
        term.write(`\r\n\x1b[2m[${t("terminalExited")}]\x1b[0m\r\n`);
        source?.close();
      });
    };

    (async () => {
      try {
        if (!current) {
          current = (await api.terminalOpen(term.cols, term.rows)).id;
          if (closed) return;
          onId(current);
        } else {
          await api.terminalResize(current, term.cols, term.rows);
        }
        attach(current);
        term.focus();
      } catch (error) {
        onError(error);
        onId(null);
      }
    })();

    const observer = new ResizeObserver(() => {
      fit.fit();
      if (current) api.terminalResize(current, term.cols, term.rows).catch(() => {});
    });
    observer.observe(host.current);

    return () => {
      closed = true;
      observer.disconnect();
      input.dispose();
      source?.close();
      if (timer) clearTimeout(timer);
      term.dispose();
    };
    // The shell outlives re-renders; a new one starts only when id is cleared.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id === null]);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex items-center gap-0.5 px-1.5 py-1">
        <span className="flex-1 px-1 text-[13px] text-ink-2">{t("terminal")}</span>
        {exited && (
          <IconButton title={t("terminalNew")} onClick={() => { setExited(false); onId(null); }}>
            <Plus size={15} />
          </IconButton>
        )}
        <IconButton
          title={t("terminalClose")}
          onClick={() => {
            if (id) api.terminalClose(id).catch(() => {});
            setExited(false);
            onId(null);
          }}
        >
          <X size={15} />
        </IconButton>
      </div>
      <div ref={host} className="min-h-0 flex-1 px-2 pb-2" />
    </div>
  );
}
