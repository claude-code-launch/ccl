import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Check, ChevronDown } from "lucide-react";
import type { ReactNode } from "react";
import type { ModelChoice } from "../types";

// Both mobile selectors share their placement, dismissal, keyboard behavior
// and visual treatment so configuration and model menus stay consistent.
export function MobileMenu({ label, heading = label, title, disabled, triggerContent, className, align = "end", width = 264, minWidth = 0, compact, children }: {
  label: string;
  heading?: string;
  title: string;
  disabled: boolean;
  triggerContent: ReactNode;
  className: string;
  align?: "start" | "end";
  width?: number;
  minWidth?: number;
  compact?: boolean;
  children: (close: () => void) => ReactNode;
}) {
  const id = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const menu = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState({ bottom: 0, left: 12, width, maxHeight: 600 });
  const expanded = open && !disabled;

  useLayoutEffect(() => {
    if (!expanded) return;
    const place = () => {
      const rect = trigger.current?.getBoundingClientRect();
      if (!rect) return;
      const menuWidth = Math.min(width, window.innerWidth - 24);
      setPosition({
        bottom: window.innerHeight - rect.top + 8,
        left: Math.max(12, Math.min(align === "start" ? rect.left : rect.right - menuWidth, window.innerWidth - menuWidth - 12)),
        width: menuWidth,
        maxHeight: Math.max(80, Math.min(600, rect.top - (window.visualViewport?.offsetTop || 0) - 20)),
      });
    };
    place();
    window.addEventListener("resize", place);
    window.addEventListener("scroll", place, true);
    window.visualViewport?.addEventListener("resize", place);
    const selected = menu.current?.querySelector<HTMLButtonElement>('[aria-checked="true"]');
    selected?.focus({ preventScroll: true });
    selected?.scrollIntoView({ block: "nearest" });
    return () => {
      window.removeEventListener("resize", place);
      window.removeEventListener("scroll", place, true);
      window.visualViewport?.removeEventListener("resize", place);
    };
  }, [expanded, align, width]);

  useEffect(() => {
    if (!expanded) return;
    const dismiss = (event: PointerEvent) => {
      const target = event.target as Node;
      if (!menu.current?.contains(target) && !trigger.current?.contains(target)) setOpen(false);
    };
    document.addEventListener("pointerdown", dismiss);
    return () => document.removeEventListener("pointerdown", dismiss);
  }, [expanded]);

  useEffect(() => { if (disabled) setOpen(false); }, [disabled]);

  const close = () => {
    setOpen(false);
    trigger.current?.focus({ preventScroll: true });
  };
  return (
    <div style={{ minWidth }} className={"sm:hidden " + className}>
      <button
        ref={trigger}
        type="button"
        aria-label={label}
        aria-haspopup="menu"
        aria-expanded={expanded}
        aria-controls={expanded ? id : undefined}
        title={title}
        disabled={disabled}
        onClick={() => setOpen(!open)}
        className={"flex min-h-11 max-w-full items-center rounded-lg text-[14px] text-ink-2 outline-none hover:bg-hover focus-visible:ring-2 focus-visible:ring-clay/40 disabled:opacity-60 " + (compact ? "size-11 justify-center" : "gap-1.5 px-2")}
      >
        {triggerContent}
        {!compact && <ChevronDown size={12} className="shrink-0 text-ink-3" />}
      </button>
      {expanded && createPortal(
        <div
          ref={menu}
          id={id}
          role="menu"
          aria-label={label}
          style={position}
          onKeyDown={(event) => {
            if (event.key === "Escape" || event.key === "Tab") {
              if (event.key === "Escape") event.preventDefault();
              close();
            }
            if (!["ArrowDown", "ArrowUp", "Home", "End"].includes(event.key)) return;
            event.preventDefault();
            const items = Array.from(menu.current?.querySelectorAll<HTMLButtonElement>('[role="menuitemradio"], [role="menuitem"]') || []);
            const index = items.indexOf(document.activeElement as HTMLButtonElement);
            const next = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 : (index + (event.key === "ArrowDown" ? 1 : -1) + items.length) % items.length;
            items[next]?.focus();
          }}
          className="fixed z-50 flex flex-col overflow-hidden rounded-2xl border border-line-strong bg-surface shadow-[0_8px_36px_rgba(0,0,0,0.18)] sm:hidden dark:shadow-[0_8px_36px_rgba(0,0,0,0.45)]"
        >
          <div className="shrink-0 border-b border-line bg-active/40 px-4 py-2 text-center text-[14px] font-semibold text-ink-2">{heading}</div>
          {children(close)}
        </div>, document.body,
      )}
    </div>
  );
}

export function MobileMenuOption({ text, selected, onClick, trailing }: { text: string; selected?: boolean; onClick: () => void; trailing?: ReactNode }) {
  return (
    <button
      type="button"
      role={selected === undefined ? "menuitem" : "menuitemradio"}
      aria-checked={selected}
      onClick={onClick}
      className="flex min-h-9 w-full items-center gap-2 rounded-lg px-3 py-1.5 text-left text-[15px] leading-5 text-ink-2 outline-none transition-colors hover:bg-active focus-visible:bg-active active:bg-active"
    >
      <span className="grid size-5 shrink-0 place-items-center text-ink-3">{selected && <Check size={19} strokeWidth={2} />}</span>
      <span className="min-w-0 flex-1 break-words">{text}</span>
      {trailing}
    </button>
  );
}

export function MobileChoiceMenu({ choices, value, onChange, icon, ...props }: {
  choices: ModelChoice[];
  value: string;
  onChange: (value: string) => void;
  icon?: ReactNode;
  label: string;
  title: string;
  disabled: boolean;
}) {
  const options = choices.some((choice) => choice.value === value) ? choices : [...choices, { value, label: value }];
  const label = options.find((choice) => choice.value === value)?.label;
  return (
    <MobileMenu {...props} align="start" width={320} className="max-w-[36%]" triggerContent={<>{icon}<span className="truncate">{label}</span></>}>
      {(close) => (
        <div role="group" aria-label={props.label} className="min-h-9 overflow-y-auto overscroll-contain p-1">
          {options.map((choice) => <MobileMenuOption key={choice.value} text={choice.label} selected={choice.value === value} onClick={() => { onChange(choice.value); close(); }} />)}
        </div>
      )}
    </MobileMenu>
  );
}
