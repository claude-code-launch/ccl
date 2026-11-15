import type { ProjectDefaults, ProviderView } from "./types";
import { EFFORT_LEVELS } from "./types";

export interface RouteEffortInfo {
  supportsEffort: boolean;
  effortLevels: readonly string[];
  currentEffort: string;
}

export function stripModelContextSuffix(model: string): string {
  return model.replace(/(?:\[1m\])+$/gi, "").trim();
}

export function resolveRouteEffortInfo(
  route: ProjectDefaults | null | undefined,
  providers: readonly ProviderView[],
  defaultProvider = "default",
  defaultEffortLevels: readonly string[] = EFFORT_LEVELS,
): RouteEffortInfo {
  const provName = route?.provider || defaultProvider || "default";
  if (provName === "default") {
    return {
      supportsEffort: true,
      effortLevels: defaultEffortLevels,
      currentEffort: route?.effort || "",
    };
  }
  const provider = providers.find((item) => item.name === provName);
  const choices = provider?.modelChoices || [];
  const model = stripModelContextSuffix(route?.model || provider?.defaultModel || "");
  const selectedChoice = (model ? choices.find((choice) => choice.value === model) : undefined) || choices[0];
  const supportsEffort = selectedChoice ? selectedChoice.supportsEffort !== false : true;
  const effortLevels: readonly string[] = !supportsEffort
    ? []
    : selectedChoice?.effortLevels && selectedChoice.effortLevels.length > 0
      ? selectedChoice.effortLevels
      : defaultEffortLevels;
  return {
    supportsEffort,
    effortLevels,
    currentEffort: route?.effort || "",
  };
}

export function buildSlashSuggestions(
  text: string,
  slashCommands: readonly string[],
  supportsEffort: boolean,
  effortLevels: readonly string[],
  locked: boolean,
): string[] | null {
  const effortArg = /^\/effort\s+(\S*)$/i.exec(text);
  if (effortArg) {
    if (!supportsEffort || locked) return [];
    const needle = effortArg[1].toLowerCase();
    const candidates = ["default", ...effortLevels];
    return candidates
      .filter((item) => item.toLowerCase().startsWith(needle))
      .map((item) => `effort ${item}`);
  }
  const cmdMatch = /^\/(\S*)$/.exec(text);
  if (!cmdMatch) return null;
  const needle = cmdMatch[1].toLowerCase();
  const hasEffort = slashCommands.some((name) => name.toLowerCase() === "effort");
  const all = supportsEffort && !locked && !hasEffort ? ["effort", ...slashCommands] : [...slashCommands];
  const starts = all.filter((name) => name.toLowerCase().startsWith(needle));
  const contains = all.filter(
    (name) => !name.toLowerCase().startsWith(needle) && name.toLowerCase().includes(needle),
  );
  return [...starts, ...contains].slice(0, 50);
}

export type EffortSlashResult =
  | { matched: false }
  | { matched: true; ok: true; effort: string }
  | { matched: true; ok: false };

export function parseEffortSlashCommand(
  input: string,
  supportsEffort: boolean,
  effortLevels: readonly string[],
  currentEffort = "",
): EffortSlashResult {
  const match = /^\/effort(?:\s+(.*))?$/i.exec(input.trim());
  if (!match) return { matched: false };
  if (!supportsEffort || effortLevels.length === 0) {
    return { matched: true, ok: false };
  }
  const arg = (match[1] || "").trim().toLowerCase();
  if (arg === "") {
    const cycle = ["", ...effortLevels];
    const index = cycle.findIndex((item) => item.toLowerCase() === currentEffort.toLowerCase());
    const next = cycle[(index + 1) % cycle.length];
    return { matched: true, ok: true, effort: next };
  }
  const exact = effortLevels.find((level) => level.toLowerCase() === arg);
  if (exact) {
    return { matched: true, ok: true, effort: exact };
  }
  if (arg === "default" || arg === "auto" || arg === "reset" || arg === "off") {
    return { matched: true, ok: true, effort: "" };
  }
  if (arg === "ultra") {
    const max = effortLevels.find((level) => level.toLowerCase() === "max")
      || effortLevels.find((level) => level.toLowerCase() === "xhigh");
    if (max) return { matched: true, ok: true, effort: max };
  }
  return { matched: true, ok: false };
}
