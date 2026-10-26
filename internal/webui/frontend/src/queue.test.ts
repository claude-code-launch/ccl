import { describe, expect, it } from "vitest";
import { settleQueue, steerFirst } from "./queue";
import type { Queued } from "./queue";
import type { StoredEvent } from "./types";

const item = (key: string, steered?: number): Queued => ({ key, text: key, attachments: [], steered });
const user = (seq: number, steered = false): StoredEvent => ({ seq, ts: seq, type: "user", payload: steered ? { text: "x", steered } : { text: "x" } });
const none = new Set<string>();

describe("steerFirst", () => {
  it("moves a message behind the ones already steered", () => {
    const queue = [item("a", 3), item("b"), item("c")];
    expect(steerFirst(queue, "c", 5).map((entry) => [entry.key, entry.steered])).toEqual([
      ["a", 3],
      ["c", 5],
      ["b", undefined],
    ]);
  });
});

describe("settleQueue", () => {
  it("leaves a queue without steered messages alone", () => {
    expect(settleQueue([item("a")], [user(1, true)], false, none)).toBeNull();
  });

  it("drops steered messages as Claude reads them, in order", () => {
    const queue = [item("a", 4), item("b", 4), item("c")];
    // Steered events from before the first steer do not count.
    expect(settleQueue(queue, [user(2, true), user(3)], true, none)).toBeNull();
    expect(settleQueue(queue, [user(2, true), user(5, true)], true, none)?.map((entry) => entry.key)).toEqual(["b", "c"]);
    expect(settleQueue(queue, [user(5, true), user(7, true)], true, none)?.map((entry) => entry.key)).toEqual(["c"]);
  });

  it("queues a message the turn ended without reading", () => {
    const queue = [item("a", 4), item("b")];
    expect(settleQueue(queue, [], false, none)).toEqual([item("a"), item("b")]);
    // Unless its request is still out: the server decides where it goes.
    expect(settleQueue(queue, [], false, new Set(["a"]))).toBeNull();
  });
});
