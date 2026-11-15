import { describe, expect, it } from "vitest";
import { diffSection, transcriptMarkdown } from "./export";
import { buildThread } from "./thread";
import { translator } from "./i18n";

describe("diffSection", () => {
  const t = translator("en");
  it("fences the diff with more backticks than it holds", () => {
    const section = diffSection({ base: "main", diff: "+```js\n+x\n+```\n" }, "ccl/abc", t);
    expect(section).toContain("## Changes on ccl/abc since main");
    expect(section).toContain("````diff\n+```js");
    expect(section.trimEnd().endsWith("````")).toBe(true);
  });
  it("is empty without changes", () => {
    expect(diffSection({ base: "main", diff: "\n" }, "ccl/abc", t)).toBe("");
  });
  it("exports assistant turns under Grok heading", () => {
    const items = buildThread([
      { seq: 1, type: "user", payload: { text: "hello" }, ts: 1 },
      { seq: 2, type: "token", payload: { text: "world" }, ts: 2 },
    ]);
    const md = transcriptMarkdown("Session", items, t);
    expect(md).toContain("## You");
    expect(md).toContain("## Grok");
  });
});
