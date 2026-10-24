// ccl serve may be mounted under a prefix (ccl serve --base-path /chat). The
// server fills in the page's <base href="/chat/" data-ccl-base>, and every
// absolute path the page asks for goes through appURL, so the same build works
// at the root and under any prefix. The prefix comes from that tag alone: the
// address bar of a share link or a nested view cannot be told apart from it.

// basePath is the prefix without a trailing slash: "" at the root, "/chat"
// under --base-path /chat.
export function basePath(doc: Pick<Document, "querySelector"> | undefined = globalThis.document): string {
  const href = doc?.querySelector("base[data-ccl-base]")?.getAttribute("href") || "/";
  // Only a local absolute path is a prefix; anything else falls back to the root.
  if (!href.startsWith("/") || href.startsWith("//")) return "";
  return href.replace(/\/+$/, "");
}

// appURL turns a root path such as /api/sessions?x=1 into the one this page
// is served under.
export function appURL(path: string, base = basePath()): string {
  return base + path;
}

// appPath is the address bar's path as the page sees it at the root, or null
// when it lies outside the prefix.
export function appPath(pathname: string, base = basePath()): string | null {
  if (!base) return pathname;
  if (pathname === base) return "/";
  return pathname.startsWith(base + "/") ? pathname.slice(base.length) : null;
}
