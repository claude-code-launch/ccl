import { describe, expect, it } from "vitest";
import { buildSlashSuggestions, parseEffortSlashCommand, resolveRouteEffortInfo } from "./slash";
import type { ProviderView } from "./types";

describe("slash /effort helpers", () => {
  const providers: ProviderView[] = [
    {
      name: "p1",
      type: "anthropic",
      endpoint: "",
      apiKey: "",
      hasKey: true,
      oauthProvider: "",
      effortLevel: "high",
      models: "claude-opus-4-6,gpt-4o",
      contextWindow: 0,
      active: true,
      modelChoices: [
        { value: "claude-opus-4-6", label: "claude-opus-4-6", supportsEffort: true, effortLevels: ["low", "medium", "high", "max"] },
        { value: "gpt-4o", label: "gpt-4o", supportsEffort: false },
      ],
    },
  ];

  it("resolves effort support and levels for the active route model", () => {
    expect(resolveRouteEffortInfo({ provider: "p1", model: "claude-opus-4-6", effort: "high" }, providers)).toEqual({
      supportsEffort: true,
      effortLevels: ["low", "medium", "high", "max"],
      currentEffort: "high",
    });
    expect(resolveRouteEffortInfo({ provider: "p1", model: "claude-opus-4-6[1m]", effort: "max" }, providers)).toEqual({
      supportsEffort: true,
      effortLevels: ["low", "medium", "high", "max"],
      currentEffort: "max",
    });
    expect(resolveRouteEffortInfo({ provider: "p1", model: "gpt-4o" }, providers)).toEqual({
      supportsEffort: false,
      effortLevels: [],
      currentEffort: "",
    });
    expect(resolveRouteEffortInfo({}, [{ ...providers[0], defaultModel: "gpt-4o[1m]" }], "p1").supportsEffort).toBe(false);
    expect(resolveRouteEffortInfo({ provider: "default" }, providers, "p1", ["low", "medium", "high", "xhigh", "max"])).toEqual({
      supportsEffort: true,
      effortLevels: ["low", "medium", "high", "xhigh", "max"],
      currentEffort: "",
    });
  });

  it("surfaces /effort and /effort <level> autocomplete only when supported and unlocked", () => {
    expect(buildSlashSuggestions("/ef", ["compact"], true, ["low", "high"], false)).toEqual(["effort"]);
    expect(buildSlashSuggestions("/ef", ["compact"], false, [], false)).toEqual([]);
    expect(buildSlashSuggestions("/effort ", ["compact"], true, ["low", "high"], false)).toEqual([
      "effort default",
      "effort low",
      "effort high",
    ]);
    expect(buildSlashSuggestions("/effort h", ["compact"], true, ["low", "high"], false)).toEqual(["effort high"]);
    expect(buildSlashSuggestions("hello", ["compact"], true, ["low", "high"], false)).toBeNull();
  });

  it("parses /effort commands without forwarding them to the model", () => {
    const levels = ["low", "medium", "high", "max"];
    expect(parseEffortSlashCommand("explain /effort", true, levels)).toEqual({ matched: false });
    expect(parseEffortSlashCommand("/effort high", true, levels)).toEqual({ matched: true, ok: true, effort: "high" });
    expect(parseEffortSlashCommand("/effort default", true, levels, "high")).toEqual({ matched: true, ok: true, effort: "" });
    expect(parseEffortSlashCommand("/effort ultra", true, levels)).toEqual({ matched: true, ok: true, effort: "max" });
    expect(parseEffortSlashCommand("/effort", true, levels, "")).toEqual({ matched: true, ok: true, effort: "low" });
    expect(parseEffortSlashCommand("/effort", true, levels, "max")).toEqual({ matched: true, ok: true, effort: "" });
    expect(parseEffortSlashCommand("/effort invalid", true, levels)).toEqual({ matched: true, ok: false });
    expect(parseEffortSlashCommand("/effort high", false, [])).toEqual({ matched: true, ok: false });
  });
});
