import { describe, expect, it } from "vitest";
import { appPath, appURL, basePath } from "./base";

const page = (href: string | null) => ({
  querySelector: () => (href === null ? null : ({ getAttribute: () => href } as unknown as Element)),
});

describe("basePath", () => {
  it("reads the prefix ccl serve filled in", () => {
    expect(basePath(page("/chat/"))).toBe("/chat");
    expect(basePath(page("/a/b/"))).toBe("/a/b");
  });

  it("is the root without a usable base", () => {
    expect(basePath(page("/"))).toBe("");
    expect(basePath(page(null))).toBe("");
    expect(basePath(undefined)).toBe("");
    expect(basePath(page("https://evil.example/"))).toBe("");
    expect(basePath(page("//evil.example/"))).toBe("");
  });
});

describe("appURL", () => {
  it("prefixes root paths and keeps the query", () => {
    expect(appURL("/api/sessions?since=3&token=a%2Fb", "/chat")).toBe("/chat/api/sessions?since=3&token=a%2Fb");
    expect(appURL("/health", "")).toBe("/health");
  });
});

describe("appPath", () => {
  it("strips the prefix from the address bar", () => {
    expect(appPath("/chat/share/abc", "/chat")).toBe("/share/abc");
    expect(appPath("/chat", "/chat")).toBe("/");
    expect(appPath("/share/abc", "")).toBe("/share/abc");
  });

  it("does not match a sibling of the prefix", () => {
    expect(appPath("/chatty/share/abc", "/chat")).toBeNull();
    expect(appPath("/share/abc", "/chat")).toBeNull();
  });
});
