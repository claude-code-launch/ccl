// Configuration decorators only. The upstream CLI owns stdio, ACP versions,
// capabilities, every request/notification, tools, permissions and persistence.
import { randomUUID } from "node:crypto";
import { readFile, realpath } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const controlURL = process.env.CCL_ACP_CONTROL_URL;
const controlToken = process.env.CCL_ACP_CONTROL_TOKEN;
delete process.env.CCL_ACP_CONTROL_URL;
delete process.env.CCL_ACP_CONTROL_TOKEN;

const [adapterPath, ...adapterArgs] = process.argv.slice(2);
const packageName = "@agentclientprotocol/claude-agent-acp";

async function findAdapter(cli) {
  const resolved = await realpath(cli);
  for (const anchor of new Set([resolved, cli])) {
    let directory = path.dirname(anchor);
    for (
      let depth = 0;
      depth < 6;
      depth++, directory = path.dirname(directory)
    ) {
      for (const file of [
        path.join(directory, "package.json"),
        path.join(directory, "node_modules", packageName, "package.json"),
        path.join(directory, packageName, "package.json"),
      ]) {
        let metadata;
        try {
          metadata = JSON.parse(await readFile(file, "utf8"));
        } catch {
          continue;
        }
        if (metadata.name !== packageName) continue;
        const root = path.dirname(file);
        const bin =
          typeof metadata.bin === "string"
            ? metadata.bin
            : metadata.bin?.["claude-agent-acp"];
        if (!bin) continue;
        const exported = metadata.exports?.["."]?.import ?? metadata.main;
        if (typeof exported !== "string") continue;
        return {
          entry: path.resolve(root, bin),
          library: path.resolve(root, exported),
        };
      }
    }
  }
  throw new Error(
    `Cannot locate ${packageName} from ${cli}. Install it with npm install -g ${packageName} (Node.js >=22).`,
  );
}

async function configuration(action, body, signal) {
  const response = await fetch(`${controlURL}/${action}`, {
    method: "POST",
    headers: {
      Authorization: `Bearer ${controlToken}`,
      "Content-Type": "application/json",
    },
    body: JSON.stringify(body),
    signal,
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}

async function release(lease) {
  if (!lease) return;
  await configuration("release", { leaseId: lease.leaseId }).catch(() => {});
}

async function acquire(cwd, signal) {
  const leaseId = randomUUID();
  try {
    return await configuration("acquire", { leaseId, cwd }, signal);
  } catch (error) {
    await release({ leaseId });
    throw error;
  }
}

function mergeEnv(client, configured) {
  const result = { ...client };
  for (const [key, value] of Object.entries(configured ?? {})) {
    if (process.platform === "win32") {
      for (const old of Object.keys(result))
        if (old.toLowerCase() === key.toLowerCase()) delete result[old];
    }
    result[key] = value;
  }
  return result;
}

function modelConfiguration(options) {
  return JSON.stringify({
    model: options.settings?.model,
    overrides: options.settings?.modelOverrides,
    picker: options.settings?.modelPicker,
    env: Object.fromEntries(
      Object.entries(options.env ?? {}).filter(([key]) =>
        /^ANTHROPIC_(?:MODEL$|DEFAULT_.*_MODEL$|CUSTOM_MODEL_OPTION$)/.test(
          key,
        ),
      ),
    ),
  });
}

async function inject(params, configured) {
  const options = params._meta?.claudeCode?.options ?? {};
  let clientSettings = options.settings ?? {};
  if (typeof clientSettings === "string") {
    clientSettings = JSON.parse(
      await readFile(path.resolve(params.cwd, clientSettings), "utf8"),
    );
  }
  const settings = {
    ...clientSettings,
    ...configured.settings,
    env: mergeEnv(clientSettings.env, configured.settings?.env),
  };
  for (const key of ["outputStyle", "language", "statusLine", "modelPicker"]) {
    if (clientSettings[key] != null) settings[key] = clientSettings[key];
  }
  // `ccl acp --model` is selected through upstream's configuration interface
  // (selectChosenModel), so a model a provider refuses never reaches the SDK.
  const { model: _chosen, ...sdkOptions } = configured;
  return {
    ...params,
    _meta: {
      ...params._meta,
      claudeCode: {
        ...params._meta?.claudeCode,
        options: {
          ...options,
          ...sdkOptions,
          settings,
          env: mergeEnv(options.env, configured.env),
        },
      },
    },
  };
}

const { entry, library } = await findAdapter(adapterPath);
const { ClaudeAcpAgent } = await import(pathToFileURL(library));
const prototype = ClaudeAcpAgent?.prototype;
for (const method of [
  "newSession",
  "loadSession",
  "resumeSession",
  "startTurn",
  "prompt",
  "cancel",
  "closeSession",
  "setSessionConfigOption",
]) {
  if (typeof prototype?.[method] !== "function") {
    throw new Error(
      `Unsupported claude-agent-acp API. Upgrade with npm install -g ${packageName}; CCL is tested with 0.90.0.`,
    );
  }
}

const originals = Object.fromEntries(
  Object.getOwnPropertyNames(prototype)
    .filter((name) => typeof prototype[name] === "function")
    .map((name) => [name, prototype[name]]),
);
const agents = new WeakMap();
function state(agent) {
  let value = agents.get(agent);
  if (value) return value;
  value = { sessions: new Map(), aliases: new Map(), reverse: new Map() };
  agents.set(agent, value);
  // Compatibility with ids issued by the old CCL bridge. New sessions use
  // upstream ids directly. Only the envelope's session id is translated.
  agent.client = new Proxy(agent.client, {
    get(target, key) {
      const method = Reflect.get(target, key);
      if (typeof method !== "function") return method;
      return (params, ...rest) => {
        const alias = value.reverse.get(params?.sessionId);
        return method.call(
          target,
          alias ? { ...params, sessionId: alias } : params,
          ...rest,
        );
      };
    },
  });
  return value;
}

function boundSession(connection, id) {
  return (
    connection.sessions.get(id) ??
    connection.sessions.get(connection.reverse.get(id))
  );
}

// `ccl acp --model` is selected through the upstream configuration interface,
// so the session runs it and clients show it; a loaded transcript would keep
// its own. A model upstream refuses leaves the session on its own model.
async function selectChosenModel(agent, sessionId, lease, result) {
  const value = lease.options.model;
  if (!value) return result;
  try {
    const selected = await originals.setSessionConfigOption.call(agent, {
      sessionId,
      configId: "model",
      value,
    });
    return selected?.configOptions && result.configOptions
      ? { ...result, configOptions: selected.configOptions }
      : result;
  } catch (error) {
    console.error(`ccl acp --model ${value}: ${error?.message ?? error}`);
    return result;
  }
}

for (const name of [
  "newSession",
  "loadSession",
  "resumeSession",
  "unstable_forkSession",
]) {
  const original = originals[name];
  if (!original) continue;
  prototype[name] = async function (params) {
    const connection = state(this);
    const old = boundSession(connection, params.sessionId);
    const preceding = old?.ready;
    let prepared;
    if (old && name !== "unstable_forkSession") {
      old.ready = new Promise((resolve) => {
        prepared = resolve;
      });
    }
    try {
      await preceding;
      let base = { ...old?.params, ...params };
      let clientID = params.sessionId;
      let backendID = connection.aliases.get(clientID) ?? clientID;
      const lease = await acquire(base.cwd);
      try {
        const forwarded = await inject(base, lease.options);
        if (backendID) forwarded.sessionId = backendID;
        let result = await original.call(this, forwarded);
        if (name === "newSession" || name === "unstable_forkSession") {
          clientID = result.sessionId;
          backendID = result.sessionId;
        }
        result = await selectChosenModel(this, backendID, lease, result);
        const previous = name === "unstable_forkSession" ? undefined : old;
        const previousLease = previous?.lease;
        const bound = previous ?? {
          busy: 0,
          ready: Promise.resolve(),
          preparing: new Set(),
        };
        Object.assign(bound, {
          params: structuredClone(base),
          backendID,
          lease,
        });
        connection.sessions.set(clientID, bound);
        await release(previousLease);
        return clientID !== backendID && result.sessionId
          ? { ...result, sessionId: clientID }
          : result;
      } catch (error) {
        await release(lease);
        throw error;
      }
    } finally {
      prepared?.();
    }
  };
}

// Both ACP v1 prompt and the upstream v2 foreground API call startTurn. Only
// decorate that common configuration boundary; the adapter still owns turns.
prototype.startTurn = async function (params, events) {
  const connection = state(this);
  const session = boundSession(connection, params.sessionId);
  if (!session) return originals.startTurn.call(this, params, events);
  const preceding = session.ready;
  let prepared;
  session.ready = new Promise((resolve) => {
    prepared = resolve;
  });
  const refresh = session.busy++ === 0;
  const abort = new AbortController();
  session.preparing.add(abort);
  let finished = false;
  const finish = () => {
    if (!finished) {
      finished = true;
      session.busy--;
    }
  };
  const cancelled = () => {
    finish();
    return events.ended({ stopReason: "cancelled" });
  };
  try {
    await preceding;
    if (abort.signal.aborted) return cancelled();
    if (refresh) {
      const candidate = await acquire(session.params.cwd, abort.signal);
      if (candidate.generation === session.lease.generation) {
        await release(candidate);
      } else {
        let selectedDefault;
        try {
          const configured = await inject(
            { ...session.params, sessionId: session.backendID },
            candidate.options,
          );
          if (
            modelConfiguration(candidate.options) !==
              modelConfiguration(session.lease.options) &&
            !session.params._meta?.claudeCode?.options?.model
          ) {
            configured._meta.claudeCode.options.model =
              candidate.options.settings?.model ||
              candidate.options.env?.ANTHROPIC_MODEL ||
              "sonnet";
            // The chosen model first; the new provider's default if refused.
            selectedDefault = [
              candidate.options.model,
              configured._meta.claudeCode.options.model,
            ].filter(Boolean);
          }
          // Upstream resume refreshes a query without replaying its transcript.
          // Keep the old runtime lease until that query has been replaced.
          await originals.resumeSession.call(this, configured);
        } catch (error) {
          await release(candidate);
          throw error;
        }
        const previous = session.lease;
        session.lease = candidate;
        session.pendingModel = selectedDefault;
        await release(previous);
      }
    }
    if (abort.signal.aborted) return cancelled();
    if (session.pendingModel) {
      // Claude restores the transcript's model on resume. Use the upstream
      // configuration interface to apply a changed CCL default explicitly.
      const [value, ...fallbacks] = session.pendingModel;
      try {
        await originals.setSessionConfigOption.call(this, {
          sessionId: session.backendID,
          configId: "model",
          value,
        });
      } catch (error) {
        if (!fallbacks.length) throw error;
        console.error(`ccl acp --model ${value}: ${error?.message ?? error}`);
        await originals.setSessionConfigOption.call(this, {
          sessionId: session.backendID,
          configId: "model",
          value: fallbacks[0],
        });
      }
      session.pendingModel = undefined;
    }
    session.preparing.delete(abort);
    const forwardedEvents = {
      ...events,
      ended: (outcome) => {
        finish();
        return events.ended(outcome);
      },
      failed: (error) => {
        finish();
        return events.failed(error);
      },
    };
    return await originals.startTurn.call(
      this,
      { ...params, sessionId: session.backendID },
      forwardedEvents,
    );
  } catch (error) {
    finish();
    if (abort.signal.aborted) return cancelled();
    throw error;
  } finally {
    prepared();
    session.preparing.delete(abort);
  }
};

prototype.cancel = function (params) {
  const connection = state(this);
  const session = boundSession(connection, params.sessionId);
  for (const abort of session?.preparing ?? []) abort.abort();
  return originals.cancel.call(this, {
    ...params,
    sessionId: session?.backendID ?? params.sessionId,
  });
};

// Upstream delete and archive tear a loaded session down without going through
// closeSession, so each of them must release the session's provider lease too.
for (const name of ["closeSession", "deleteSession", "archiveSession"]) {
  const original = originals[name];
  if (!original) continue;
  prototype[name] = async function (params, ...rest) {
    const connection = state(this);
    const session = boundSession(connection, params.sessionId);
    for (const abort of session?.preparing ?? []) abort.abort();
    await session?.ready;
    const result = await original.call(
      this,
      { ...params, sessionId: session?.backendID ?? params.sessionId },
      ...rest,
    );
    if (session) {
      for (const [id, bound] of connection.sessions)
        if (bound === session) connection.sessions.delete(id);
      await release(session.lease);
    }
    return result;
  };
}

// These methods are delegated without interpreting any of their fields. The
// alias is needed only by conversations created before CCL used this adapter.
for (const name of [
  "prompt",
  "setSessionMode",
  "setSessionConfigOption",
  "steer",
  "stopAsyncTask",
  "goal",
]) {
  if (!originals[name]) continue;
  prototype[name] = function (params, ...rest) {
    const backendID = state(this).aliases.get(params?.sessionId);
    return originals[name].call(
      this,
      backendID ? { ...params, sessionId: backendID } : params,
      ...rest,
    );
  };
}

process.argv = [process.execPath, entry, ...adapterArgs];
await import(pathToFileURL(entry));
