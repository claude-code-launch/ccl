import { Gauge } from "lucide-react";
import type { ModelChoice, ProjectDefaults } from "../types";
import { EFFORT_LEVELS } from "../types";
import { useI18n } from "../i18n";
import { MobileMenu, MobileMenuOption } from "./MobileMenu";

export const effortLabel = (level: string) => {
  switch (level) {
    case "xhigh":
      return "Extra High";
    case "minimal":
      return "Minimal";
    case "max":
      return "Max";
    case "ultra":
      return "Ultra";
    default:
      return level ? level.charAt(0).toUpperCase() + level.slice(1) : "";
  }
};

const effortMarks: Record<string, string> = {
  minimal: "Min",
  low: "L",
  medium: "M",
  high: "H",
  xhigh: "X",
  max: "Max",
  ultra: "U",
};

function EffortMark({ level }: { level: string }) {
  return (
    <span aria-hidden="true" className="grid h-6 min-w-6 shrink-0 place-items-center rounded-md border border-line bg-active/40 px-1 font-mono text-[11px] font-medium leading-none text-ink-2">
      {level ? effortMarks[level] || level.slice(0, 3).toUpperCase() : <Gauge size={15} />}
    </span>
  );
}

export function MobileModelMenu({ choices, model, disabled, title, onChange }: {
  choices: ModelChoice[];
  model: string;
  disabled: boolean;
  title: string;
  onChange: (patch: ProjectDefaults) => void;
}) {
  const { t } = useI18n();
  // A pool that contains a literal "auto" model would duplicate the menu's
  // own auto entry; keep one.
  const rest = choices.filter((choice) => choice.value !== "");
  const models = [{ value: "", label: t("auto") }, ...rest];
  if (model && !models.some((choice) => choice.value === model)) models.push({ value: model, label: model });
  const label = models.find((choice) => choice.value === model)?.label || t("auto");
  return (
    <MobileMenu label={t("model")} title={title} disabled={disabled} className="flex-1" minWidth={44} triggerContent={<span className="truncate">{label}</span>}>
      {(close) => (
        <div role="group" aria-label={t("model")} className="min-h-9 overflow-y-auto overscroll-contain p-1">
          {models.map((choice) => <MobileMenuOption key={choice.value} text={choice.label} selected={model === choice.value} onClick={() => { onChange({ model: choice.value }); close(); }} />)}
        </div>
      )}
    </MobileMenu>
  );
}

export function MobileEffortMenu({ effort, levels: propLevels = EFFORT_LEVELS, disabled, title, onChange }: {
  effort: string;
  levels?: readonly string[];
  disabled: boolean;
  title: string;
  onChange: (patch: ProjectDefaults) => void;
}) {
  const { t } = useI18n();
  const levels = [
    { value: "", label: t("effortDefault") },
    ...[...propLevels].reverse().map((level) => ({ value: level, label: effortLabel(level) })),
  ];
  if (effort && !levels.some((level) => level.value === effort)) {
    levels.push({ value: effort, label: effortLabel(effort) });
  }
  const current = levels.find((level) => level.value === effort)?.label || effortLabel(effort);
  return (
    <MobileMenu label={t("effort")} heading={t("reasoning")} title={`${t("reasoning")} · ${current} · ${title}`} disabled={disabled} className="order-11 shrink-0" width={200} compact triggerContent={<EffortMark level={effort} />}>
      {(close) => (
        <div role="group" aria-label={t("reasoning")} className="min-h-9 overflow-y-auto overscroll-contain p-1">
          {levels.map((level) => <MobileMenuOption key={level.value} text={level.label} selected={effort === level.value} trailing={<EffortMark level={level.value} />} onClick={() => { onChange({ effort: level.value }); close(); }} />)}
        </div>
      )}
    </MobileMenu>
  );
}
