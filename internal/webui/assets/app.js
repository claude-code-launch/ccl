"use strict";

const byID = (id) => document.getElementById(id);
const ui = Object.fromEntries([
  "provider", "workspace", "create", "create-button", "count", "sessions", "title", "path",
  "status", "status-label", "transcript", "empty", "empty-hint", "messages", "approvals",
  "error", "composer", "prompt", "stop", "send",
].map((id) => [id, byID(id)]));

const fragment = location.hash.slice(1);
const token = fragment || sessionStorage.getItem("ccl-grok-token");
if (fragment) {
  sessionStorage.setItem("ccl-grok-token", fragment);
  history.replaceState(null, "", location.pathname);
}
window.addEventListener("hashchange", () => {
  if (location.hash) location.reload();
});

let sessions = [];
let view;
const drafts = new Map();
const stateNames = {
  idle: "Ready", working: "Working", loading: "Loading history",
  stopping: "Stopping", connecting: "Connecting", offline: "Reconnecting",
};

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text) node.textContent = text;
  return node;
}

function workspaceName(path) {
  return path.split(/[/\\]/).filter(Boolean).at(-1) || path;
}

function showError(message) {
  ui.error.textContent = message;
  ui.error.hidden = !message;
}

function updateControls() {
  const state = view?.state;
  ui.prompt.disabled = !view || state === "connecting" || state === "offline" || state === "loading";
  ui.send.disabled = state !== "idle" || view.submitting || !ui.prompt.value.trim();
  ui.stop.hidden = state !== "working" && state !== "loading" && state !== "stopping";
  ui.stop.disabled = state === "stopping" || view?.canceling;
}

function setState(state) {
  if (view) view.state = state;
  ui.status.dataset.state = state;
  ui["status-label"].textContent = stateNames[state] || state;
  updateControls();
}

async function api(path, body, signal) {
  const headers = { Authorization: "Bearer " + token };
  const options = { headers, signal, credentials: "omit" };
  if (body !== undefined) {
    options.method = "POST";
    headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(body);
  }
  const response = await fetch("/api/" + path, options);
  if (!response.ok) {
    const error = new Error((await response.text()).trim());
    error.status = response.status;
    throw error;
  }
  return response;
}

function renderSessions() {
  ui.count.textContent = String(sessions.length).padStart(2, "0");
  ui.sessions.replaceChildren();
  sessions.forEach((session, index) => {
    const button = element("button", "session");
    button.type = "button";
    button.setAttribute("aria-current", String(view?.session.id === session.id));
    const summary = element("span");
    const title = element("strong", "", session.title);
    title.title = session.title;
    const workspace = element("small", "", workspaceName(session.workspace));
    workspace.title = session.workspace;
    summary.append(title, workspace);
    button.append(element("span", "serial", String(index + 1).padStart(2, "0")), summary);
    button.addEventListener("click", () => selectSession(session));
    ui.sessions.append(button);
  });
}

function selectSession(session) {
  if (view?.session.id === session.id) return;
  view?.controller.abort();
  const selected = {
    session, controller: new AbortController(), tools: new Map(), permissions: new Map(),
  };
  view = selected;
  sessionStorage.setItem("ccl-grok-session", session.id);
  ui.title.textContent = session.title;
  ui.path.textContent = session.workspace;
  ui.prompt.value = drafts.get(session.id) || "";
  ui["empty-hint"].textContent = "Send a message to begin working in this directory.";
  resetTranscript(selected);
  renderSessions();
  showError("");
  setState("connecting");
  follow(selected);
}

function resetTranscript(selected) {
  selected.last = undefined;
  selected.tools.clear();
  selected.permissions.clear();
  ui.messages.replaceChildren();
  ui.approvals.replaceChildren();
  ui.approvals.hidden = true;
  ui.empty.hidden = false;
}

function appendMessage(selected, event) {
  let last = selected.last;
  if (!event.append || last?.role !== event.kind) {
    const article = element("article", "message " + event.kind);
    const label = element("header", "", event.kind === "user" ? "YOU" : "GROK BUILD");
    const body = element("div", "message-text");
    const text = document.createTextNode("");
    body.append(text);
    article.append(label, body);
    ui.messages.append(article);
    last = selected.last = { role: event.kind, text };
  }
  last.text.appendData(event.text || "");
}

function renderTool(selected, event) {
  let tool = selected.tools.get(event.id);
  if (!tool) {
    const row = element("div", "tool");
    tool = { title: element("span", "", "Tool"), status: element("span", "tool-state") };
    selected.tools.set(event.id, tool);
    row.append(tool.title, tool.status);
    ui.messages.append(row);
  }
  if (event.text) tool.title.textContent = event.text;
  if (event.status) tool.status.textContent = event.status.replaceAll("_", " ");
}

function renderApproval(selected, event) {
  const card = element("article", "approval");
  card.append(element("p", "eyebrow", "TOOL APPROVAL"), element("h3", "", event.text));
  if (event.detail) card.append(element("pre", "", event.detail));
  const actions = element("div", "actions");
  const reject = element("button", "choice", "Deny");
  const allow = element("button", "primary", "Allow once");
  allow.disabled = !event.can_allow;
  async function decide(decision) {
    allow.disabled = reject.disabled = true;
    showError("");
    try {
      await api("sessions/" + selected.session.id + "/permissions/" + event.id, { decision });
    } catch (error) {
      if (view === selected) showError(error.message);
      reject.disabled = false;
      allow.disabled = !event.can_allow;
    }
  }
  reject.addEventListener("click", () => decide("deny"));
  allow.addEventListener("click", () => decide("allow"));
  actions.append(reject, allow);
  card.append(actions);
  selected.permissions.set(event.id, card);
  ui.approvals.append(card);
  ui.approvals.hidden = false;
}

function applyBatch(selected, batch) {
  const bottom = ui.transcript.scrollHeight - ui.transcript.scrollTop - ui.transcript.clientHeight < 100;
  for (const event of batch) {
    switch (event.kind) {
      case "state":
        setState(event.text);
        break;
      case "title":
        if (selected.session.title !== event.text) {
          selected.session.title = event.text;
          ui.title.textContent = event.text;
          renderSessions();
        }
        break;
      case "user":
      case "assistant":
        appendMessage(selected, event);
        break;
      case "tool":
        selected.last = undefined;
        renderTool(selected, event);
        break;
      case "notice": {
        selected.last = undefined;
        const notice = element("p", "notice", event.text);
        notice.dataset.status = event.status || "";
        ui.messages.append(notice);
        break;
      }
      case "permission":
        renderApproval(selected, event);
        break;
      case "permission_closed":
        selected.permissions.get(event.id)?.remove();
        selected.permissions.delete(event.id);
        ui.approvals.hidden = selected.permissions.size === 0;
        break;
    }
  }
  ui.empty.hidden = ui.messages.childElementCount !== 0;
  if (bottom) ui.transcript.scrollTop = ui.transcript.scrollHeight;
}

async function follow(selected) {
  const { signal } = selected.controller;
  while (!signal.aborted) {
    try {
      const response = await api("sessions/" + selected.session.id + "/events", undefined, signal);
      if (view !== selected) return;
      resetTranscript(selected);
      showError("");
      const stream = response.body.pipeThrough(new TextDecoderStream());
      let buffer = "";
      for await (const chunk of stream) {
        if (view !== selected) return;
        buffer += chunk;
        let boundary;
        while ((boundary = buffer.indexOf("\n\n")) >= 0) {
          const frame = buffer.slice(0, boundary);
          buffer = buffer.slice(boundary + 2);
          if (frame.startsWith("data: ")) applyBatch(selected, JSON.parse(frame.slice(6)));
        }
      }
    } catch (error) {
      if (signal.aborted) return;
      if (error.status === 401 || error.status === 404) {
        showError(error.message);
        setState("offline");
        return;
      }
    }
    if (view !== selected || signal.aborted) return;
    setState("offline");
    await new Promise((resolve) => setTimeout(resolve, 900));
  }
}

ui.create.addEventListener("submit", async (event) => {
  event.preventDefault();
  ui["create-button"].disabled = true;
  showError("");
  try {
    const response = await api("sessions", { workspace: ui.workspace.value });
    const session = await response.json();
    sessions.unshift(session);
    selectSession(session);
  } catch (error) {
    showError(error.message);
  } finally {
    ui["create-button"].disabled = false;
  }
});

ui.prompt.addEventListener("input", () => {
  if (view) drafts.set(view.session.id, ui.prompt.value);
  updateControls();
});
ui.prompt.addEventListener("keydown", (event) => {
  if (event.key === "Enter" && (event.metaKey || event.ctrlKey) && !event.isComposing) {
    event.preventDefault();
    ui.composer.requestSubmit();
  }
});
ui.composer.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (ui.send.disabled) return;
  const selected = view;
  const text = ui.prompt.value;
  selected.submitting = true;
  updateControls();
  showError("");
  try {
    await api("sessions/" + selected.session.id + "/prompt", { text });
    if (drafts.get(selected.session.id) === text) {
      drafts.delete(selected.session.id);
      if (view === selected) ui.prompt.value = "";
    }
  } catch (error) {
    if (view === selected) showError(error.message);
  } finally {
    selected.submitting = false;
    updateControls();
  }
});
ui.stop.addEventListener("click", async () => {
  const selected = view;
  selected.canceling = true;
  updateControls();
  try {
    await api("sessions/" + selected.session.id + "/cancel", {});
  } catch (error) {
    if (view === selected) showError(error.message);
  } finally {
    selected.canceling = false;
    updateControls();
  }
});

async function init() {
  if (!token) throw new Error("Open the session link printed by ccl serve.");
  const response = await api("sessions");
  const data = await response.json();
  ui.provider.textContent = "PROVIDER / " + data.provider;
  for (const workspace of data.workspaces) {
    const name = workspaceName(workspace);
    const label = data.workspaces.some((other) => other !== workspace && workspaceName(other) === name) ? workspace : name;
    const option = element("option", "", label);
    option.value = workspace;
    option.title = workspace;
    ui.workspace.append(option);
  }
  ui.workspace.disabled = ui["create-button"].disabled = false;
  sessions = data.sessions;
  const recent = sessionStorage.getItem("ccl-grok-session");
  const session = sessions.find((session) => session.id === recent) || sessions[0];
  if (session) selectSession(session);
  else {
    renderSessions();
    setState("idle");
  }
}

init().catch((error) => {
  showError(error.message);
  ui["status-label"].textContent = "Link required";
});
