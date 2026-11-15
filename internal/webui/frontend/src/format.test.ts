import { describe, expect, it } from "vitest";
import { groupSessions } from "./format";
import type { Session } from "./types";

const at = (id: string, updatedAt: string, pinned = false) => ({ id, updatedAt, pinned }) as Session;

describe("groupSessions", () => {
  it("puts pinned first and the rest by day, skipping empty groups", () => {
    const now = new Date(2026, 9, 22, 15, 0);
    const groups = groupSessions(
      [
        at("a", new Date(2026, 9, 22, 9, 0).toISOString()),
        at("b", new Date(2026, 9, 21, 23, 0).toISOString()),
        at("c", new Date(2026, 9, 1).toISOString(), true),
        at("d", new Date(2026, 8, 1).toISOString()),
      ],
      now,
    );
    expect(groups.map(([label, list]) => [label, list.map((session) => session.id)])).toEqual([
      ["pinned", ["c"]],
      ["today", ["a"]],
      ["yesterday", ["b"]],
      ["older", ["d"]],
    ]);
  });
});
