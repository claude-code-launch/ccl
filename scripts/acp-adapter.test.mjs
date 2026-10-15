import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, mkdir, writeFile, rm } from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import test from "node:test";
import { fileURLToPath } from "node:url";

const wrapper = fileURLToPath(
  new URL("../internal/acp/adapter.mjs", import.meta.url),
);

// Only the fixture owns this test protocol. Production CCL runs the actual
// upstream entry and never reads, writes or switches on an ACP frame.
const library = `
export class ClaudeAcpAgent {
  constructor(client) { this.client = client; this.sessions = new Map(); this.sequence = 0; this.resumes = 0; this.held = new Map(); }
  async newSession(params) { const sessionId = 'session-' + ++this.sequence; this.sessions.set(sessionId, params); return { sessionId, captured: params, controlLeaked: !!process.env.CCL_ACP_CONTROL_TOKEN, argv: process.argv.slice(2) }; }
  async loadSession(params) { this.sessions.set(params.sessionId, params); await this.client.sessionUpdate({ sessionId: params.sessionId, update: { sessionUpdate: 'user_message_chunk', content: { type: 'text', text: 'upstream-replay' } } }); return { sessionId: params.sessionId, captured: params }; }
  async resumeSession(params) { if (params._meta.claudeCode.options.env.REFUSE === '1') throw new Error('upstream-reload-failed'); this.resumes++; this.sessions.set(params.sessionId, params); return { sessionId: params.sessionId }; }
  async prompt(params) { let resolve, reject; const outcome = new Promise((a, b) => { resolve = a; reject = b; }); await this.startTurn(params, { inserted() {}, ended: resolve, failed: reject }); return outcome; }
  async startTurn(params, events) {
    const state = this.sessions.get(params.sessionId);
    if (params.prompt?.[0]?.text === 'hold') { await this.client.sessionUpdate({ sessionId: params.sessionId, update: { sessionUpdate: 'fixture-held' } }); this.held.set(params.sessionId, events.ended); return; }
    events.ended({ stopReason: 'upstream-only', captured: params, options: state._meta.claudeCode.options, resumes: this.resumes });
  }
  async cancel(params) { this.held.get(params.sessionId)?.({ stopReason: 'cancelled' }); this.held.delete(params.sessionId); }
  async closeSession(params) { this.sessions.delete(params.sessionId); return { upstreamClose: true }; }
  async deleteSession(params) { this.sessions.delete(params.sessionId); return { upstreamDelete: true }; }
  async archiveSession(params) { this.sessions.delete(params.sessionId); return { upstreamArchive: true }; }
  async setSessionMode(params) { return { modeFromUpstream: params }; }
  async setSessionConfigOption(params) { return { configFromUpstream: params }; }
}
`;

const entry = `
import readline from 'node:readline';
import { ClaudeAcpAgent } from './lib.js';
const pending = new Map();
const send = value => process.stdout.write(JSON.stringify(value) + '\\n');
const agent = new ClaudeAcpAgent({
  sessionUpdate: params => send({ jsonrpc: '2.0', method: 'session/update', params }),
  requestPermission: params => new Promise(resolve => { pending.set('upstream:opaque', resolve); send({ jsonrpc: '2.0', id: 'upstream:opaque', method: 'session/request_permission', params }); }),
});
const methods = { 'session/new': 'newSession', 'session/load': 'loadSession', 'session/resume': 'resumeSession', 'session/prompt': 'prompt', 'session/cancel': 'cancel', 'session/close': 'closeSession', 'session/delete': 'deleteSession', '_session/archive': 'archiveSession', 'session/set_mode': 'setSessionMode', 'session/set_config_option': 'setSessionConfigOption' };
const input = readline.createInterface({ input: process.stdin });
input.on('line', async line => {
  const request = JSON.parse(line);
  if (!request.method) { pending.get(request.id)?.(request.result); return; }
  try {
    let result;
    if (request.method === 'initialize') result = { protocolVersion: 42, agentCapabilities: { futureFeature: { enabled: false, values: [null, 1, 'x'] } }, agentInfo: { name: 'upstream-fixture' }, _meta: { future: { exact: true } } };
    else if (request.method === '_future/error') { send({ jsonrpc: '2.0', id: request.id, error: { code: 7193, message: 'upstream error', data: request.params }, _meta: { upstream: 'preserved' } }); return; }
    else if (request.method === '_future/permission') result = await agent.client.requestPermission(request.params);
    else if (request.method === '_future/start-turn') result = await new Promise((resolve, reject) => { agent.startTurn(request.params, { inserted() {}, ended: resolve, failed: reject }).catch(reject); });
    else if (methods[request.method]) result = await agent[methods[request.method]](request.params);
    else result = { upstreamMethod: request.method, params: request.params };
    if ('id' in request) send({ jsonrpc: '2.0', id: request.id, result, _meta: { upstream: 'preserved' } });
  } catch (error) { if ('id' in request) send({ jsonrpc: '2.0', id: request.id, error: { code: -32123, message: error.message, data: { upstream: true } } }); }
});
`;

async function fixture(t) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "ccl ACP fixture "));
  const root = path.join(
    directory,
    "node_modules",
    "@agentclientprotocol",
    "claude-agent-acp",
  );
  await mkdir(path.join(root, "dist"), { recursive: true });
  await writeFile(
    path.join(root, "package.json"),
    JSON.stringify({
      name: "@agentclientprotocol/claude-agent-acp",
      type: "module",
      bin: { "claude-agent-acp": "dist/index.js" },
      exports: { ".": { import: "./dist/lib.js" } },
    }),
  );
  await writeFile(path.join(root, "dist/lib.js"), library);
  await writeFile(path.join(root, "dist/index.js"), entry);
  const state = {
    generation: 1,
    released: [],
    acquired: [],
    leases: new Map(),
    refuse: false,
    pause: false,
    legacy: {},
  };
  const server = http.createServer(async (request, response) => {
    assert.equal(request.headers.authorization, "Bearer fixture-control");
    let body = "";
    for await (const chunk of request) body += chunk;
    const params = JSON.parse(body);
    response.setHeader("Content-Type", "application/json");
    if (request.url === "/release") {
      state.released.push(params.leaseId);
      state.leases.delete(params.leaseId);
      response.end("{}");
    } else if (request.url === "/legacy")
      response.end(JSON.stringify(state.legacy[params.sessionId] ?? {}));
    else {
      state.acquired.push(params);
      if (state.pause) return;
      const options = {
        env: {
          ANTHROPIC_AUTH_TOKEN: "fake-" + state.generation,
          GENERATION: String(state.generation),
          REFUSE: state.refuse ? "1" : "0",
        },
        settings: {
          model: "model-" + state.generation,
          language: "English",
          outputStyle: "Concise",
          env: { ANTHROPIC_BASE_URL: "http://fixture-" + state.generation },
        },
      };
      state.leases.set(params.leaseId, state.generation);
      response.end(
        JSON.stringify({
          leaseId: params.leaseId,
          generation: state.generation,
          options,
        }),
      );
    }
  });
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const child = spawn(
    process.execPath,
    [wrapper, path.join(root, "dist/index.js"), "--upstream-option"],
    {
      env: {
        ...process.env,
        CCL_ACP_CONTROL_URL: `http://127.0.0.1:${server.address().port}`,
        CCL_ACP_CONTROL_TOKEN: "fixture-control",
      },
      stdio: ["pipe", "pipe", "pipe"],
      windowsHide: true,
    },
  );
  let stderr = "";
  child.stderr.on("data", (chunk) => {
    stderr += chunk;
  });
  const pending = new Map();
  const notifications = [];
  const notifyWaiters = [];
  let requestSequence = 0;
  readline.createInterface({ input: child.stdout }).on("line", (line) => {
    const frame = JSON.parse(line);
    if (frame.method) {
      notifications.push(frame);
      if (frame.id)
        child.stdin.write(
          JSON.stringify({
            jsonrpc: "2.0",
            id: frame.id,
            result: {
              outcome: { chosen: "upstream" },
              arbitrary: [null, false],
            },
          }) + "\n",
        );
      for (const check of notifyWaiters) check();
    } else pending.get(JSON.stringify(frame.id))?.(frame);
  });
  const exit = once(child, "exit");
  t.after(async () => {
    child.stdin.end();
    const timer = setTimeout(() => child.kill(), 2000);
    await exit;
    clearTimeout(timer);
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    await rm(directory, { recursive: true, force: true });
  });
  function call(method, params = {}, id = ++requestSequence) {
    return new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(new Error(`timeout on ${method}: ${stderr}`)),
        5000,
      );
      pending.set(JSON.stringify(id), (frame) => {
        clearTimeout(timer);
        pending.delete(JSON.stringify(id));
        resolve(frame);
      });
      child.stdin.write(
        JSON.stringify({ jsonrpc: "2.0", id, method, params }) + "\n",
      );
    });
  }
  const notify = (method, params) =>
    child.stdin.write(
      JSON.stringify({ jsonrpc: "2.0", method, params }) + "\n",
    );
  function waitFor(predicate) {
    if (predicate()) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timer = setTimeout(
        () => reject(new Error("timeout waiting for upstream update")),
        5000,
      );
      notifyWaiters.push(() => {
        if (predicate()) {
          clearTimeout(timer);
          resolve();
        }
      });
    });
  }
  return { state, directory, call, notify, notifications, waitFor };
}

test("upstream protocol, future extensions and bidirectional requests pass through", async (t) => {
  const f = await fixture(t);
  const initialized = await f.call("initialize");
  assert.equal(initialized.result.protocolVersion, 42);
  assert.deepEqual(initialized.result.agentCapabilities, {
    futureFeature: { enabled: false, values: [null, 1, "x"] },
  });
  assert.equal(initialized.result.agentInfo.name, "upstream-fixture");
  const params = {
    arbitrary: { text: '中文 & quoted "value"', missing: null },
    values: [0, false],
  };
  const echoed = await f.call(
    "_future/unimplemented-in-ccl",
    params,
    "client/opaque-id",
  );
  assert.equal(echoed.id, "client/opaque-id");
  assert.deepEqual(echoed.result.params, params);
  assert.deepEqual(echoed._meta, { upstream: "preserved" });
  const error = await f.call("_future/error", params);
  assert.deepEqual(error.error, {
    code: 7193,
    message: "upstream error",
    data: params,
  });
  const permission = await f.call("_future/permission", {
    sessionId: "any",
    futurePermission: params,
  });
  assert.deepEqual(permission.result, {
    outcome: { chosen: "upstream" },
    arbitrary: [null, false],
  });
  assert.equal(f.notifications.at(-1).id, "upstream:opaque");
});

test("configuration injection preserves client settings, hooks and metadata", async (t) => {
  const f = await fixture(t);
  await writeFile(
    path.join(f.directory, "client.json"),
    JSON.stringify({
      language: "French",
      hooks: { Stop: [{ hooks: [] }] },
      env: { CLIENT: "kept", ANTHROPIC_BASE_URL: "http://wrong" },
    }),
  );
  const response = await f.call("session/new", {
    cwd: f.directory,
    mcpServers: [
      { name: "client-mcp", type: "http", url: "http://client.invalid" },
    ],
    _meta: {
      future: { kept: true },
      claudeCode: {
        emitRawSDKMessages: true,
        options: {
          maxTurns: 7,
          env: { CLIENT: "kept", ANTHROPIC_AUTH_TOKEN: "wrong" },
          settings: "client.json",
        },
      },
    },
  });
  assert.ifError(response.error);
  const captured = response.result.captured;
  const options = captured._meta.claudeCode.options;
  assert.deepEqual(captured._meta.future, { kept: true });
  assert.equal(captured._meta.claudeCode.emitRawSDKMessages, true);
  assert.equal(captured.mcpServers[0].name, "client-mcp");
  assert.equal(options.maxTurns, 7);
  assert.equal(options.settings.language, "French");
  assert.deepEqual(options.settings.hooks, { Stop: [{ hooks: [] }] });
  assert.equal(options.settings.env.CLIENT, "kept");
  assert.equal(options.settings.env.ANTHROPIC_BASE_URL, "http://fixture-1");
  assert.equal(options.env.ANTHROPIC_AUTH_TOKEN, "fake-1");
  assert.equal(response.result.controlLeaked, false);
  assert.deepEqual(response.result.argv, ["--upstream-option"]);
});

test("provider refresh uses upstream resume, preserves running turns and releases leases", async (t) => {
  const f = await fixture(t);
  const created = await f.call("session/new", {
    cwd: f.directory,
    mcpServers: [],
  });
  const sessionId = created.result.sessionId;
  const prompt = (text) =>
    f.call("session/prompt", { sessionId, prompt: [{ type: "text", text }] });
  const held = prompt("hold");
  await f.waitFor(() =>
    f.notifications.some(
      (n) => n.params.update.sessionUpdate === "fixture-held",
    ),
  );
  f.state.generation = 2;
  const queued = await prompt("while-running");
  assert.equal(queued.result.options.env.GENERATION, "1");
  assert.equal(queued.result.resumes, 0);
  f.notify("session/cancel", { sessionId });
  assert.equal((await held).result.stopReason, "cancelled");
  const next = await prompt("after-cancel");
  assert.equal(next.result.options.env.GENERATION, "2");
  assert.equal(next.result.options.model, "model-2");
  assert.equal(next.result.resumes, 1);
  assert.equal(
    f.notifications.filter(
      (n) => n.params?.update?.content?.text === "upstream-replay",
    ).length,
    0,
  );
  assert.equal(f.state.leases.size, 1);
  const closed = await f.call("session/close", { sessionId });
  assert.deepEqual(closed.result, { upstreamClose: true });
  assert.equal(f.state.leases.size, 0);
});

test("failed refresh keeps the previous lease and can be retried", async (t) => {
  const f = await fixture(t);
  const created = await f.call("session/new", {
    cwd: f.directory,
    mcpServers: [],
  });
  const sessionId = created.result.sessionId;
  f.state.generation = 2;
  f.state.refuse = true;
  const params = { sessionId, prompt: [{ type: "text", text: "test" }] };
  const failed = await f.call("session/prompt", params);
  assert.equal(failed.error.message, "upstream-reload-failed");
  assert.deepEqual(failed.error.data, { upstream: true });
  assert.deepEqual([...f.state.leases.values()], [1]);
  f.state.refuse = false;
  const retried = await f.call("session/prompt", params);
  assert.equal(retried.result.options.env.GENERATION, "2");
  assert.deepEqual([...f.state.leases.values()], [2]);
});

test("upstream foreground turns and repeated loads retain their active configuration lease", async (t) => {
  const f = await fixture(t);
  const created = await f.call("session/new", {
    cwd: f.directory,
    mcpServers: [],
  });
  const sessionId = created.result.sessionId;
  const oldLease = [...f.state.leases.keys()][0];
  const loaded = await f.call("session/load", {
    sessionId,
    cwd: f.directory,
    mcpServers: [],
  });
  assert.ifError(loaded.error);
  assert.equal(f.state.leases.size, 1);
  assert.ok(f.state.released.includes(oldLease));
  const activeLease = [...f.state.leases.keys()][0];
  assert.ok(!f.state.released.includes(activeLease));
  f.state.generation = 2;
  const turn = await f.call("_future/start-turn", {
    sessionId,
    prompt: [{ type: "text", text: "test" }],
  });
  assert.equal(turn.result.options.env.GENERATION, "2");
  assert.deepEqual([...f.state.leases.values()], [2]);
  await f.call("session/close", { sessionId });
  assert.equal(f.state.leases.size, 0);
});

test("delete and archive release the configuration lease of a loaded session", async (t) => {
  const f = await fixture(t);
  for (const [method, result] of [
    ["session/delete", { upstreamDelete: true }],
    ["_session/archive", { upstreamArchive: true }],
  ]) {
    const created = await f.call("session/new", {
      cwd: f.directory,
      mcpServers: [],
    });
    assert.equal(f.state.leases.size, 1);
    const ended = await f.call(method, {
      sessionId: created.result.sessionId,
    });
    assert.deepEqual(ended.result, result);
    assert.equal(f.state.leases.size, 0, method);
  }
});

test("cancel interrupts configuration preparation before a prompt is forwarded", async (t) => {
  const f = await fixture(t);
  const created = await f.call("session/new", {
    cwd: f.directory,
    mcpServers: [],
  });
  const sessionId = created.result.sessionId;
  f.state.pause = true;
  const before = f.state.acquired.length;
  const prompt = f.call("session/prompt", {
    sessionId,
    prompt: [{ type: "text", text: "test" }],
  });
  while (f.state.acquired.length === before)
    await new Promise((resolve) => setTimeout(resolve, 5));
  f.notify("session/cancel", { sessionId });
  assert.equal((await prompt).result.stopReason, "cancelled");
  assert.equal(f.state.leases.size, 1);
  f.state.pause = false;
});

test("old CCL ids load through upstream persistence and preserve client ids", async (t) => {
  const f = await fixture(t);
  f.state.legacy["old-ccl-id"] = {
    sessionId: "real-claude-id",
    cwd: f.directory,
  };
  const loaded = await f.call("session/load", {
    sessionId: "old-ccl-id",
    mcpServers: [],
  });
  assert.equal(loaded.result.sessionId, "old-ccl-id");
  assert.equal(loaded.result.captured.sessionId, "real-claude-id");
  assert.equal(f.notifications[0].params.sessionId, "old-ccl-id");
  const prompt = await f.call("session/prompt", {
    sessionId: "old-ccl-id",
    prompt: [{ type: "text", text: "test" }],
  });
  assert.equal(prompt.result.captured.sessionId, "real-claude-id");
});
