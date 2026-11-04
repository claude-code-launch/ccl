import { Gauge } from "lucide-react";
import type { ModelChoice, ProjectDefaults } from "../types";
import { EFFORT_LEVELS } from "../types";
import { useI18n } from "../i18n";
import { MobileMenu, MobileMenuOption } from "./MobileMenu";

const effortLabel = (level: string) => level === "xhigh" ? "Extra High" : level.charAt(0).toUpperCase() + level.slice(1);
const effortMarks: Record<string, string> = { low: "L", medium: "M", high: "H", xhigh: "X", max: "MAX" };

function EffortMark({ level }: { level: string }) {
  return (
    <span aria-hidden="true" className="grid h-6 min-w-6 shrink-0 place-items-center rounded-md border border-line bg-active/40 px-1 font-mono text-[11px] font-medium leading-none text-ink-2">
      {level ? effortMarks[level] || level.charAt(0).toUpperCase() : <Gauge size={15} />}
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
  const models = [{ value: "", label: t("auto") }, ...choices];
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

export function MobileEffortMenu({ effort, disabled, title, onChange }: {
  effort: string;
  disabled: boolean;
  title: string;
  onChange: (patch: ProjectDefaults) => void;
}) {
  const { t } = useI18n();
  const levels = [{ value: "", label: t("effortDefault") }, ...[...EFFORT_LEVELS].reverse().map((level) => ({ value: level, label: effortLabel(level) }))];
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
