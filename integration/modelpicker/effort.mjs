// Read the real client's menu capabilities through its public SDK. An idle
// prompt source sends no user turn; all configuration belongs to the test home.
import fs from 'node:fs';
import { pathToFileURL } from 'node:url';

const { query } = await import(pathToFileURL(process.argv[2]).href);
const settings = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
let release;
async function* idle() { await new Promise(resolve => { release = resolve; }); }
const client = query({
  prompt: idle(),
  options: {
    cwd: process.env.HOME,
    env: { ...process.env, ...settings.env },
    pathToClaudeCodeExecutable: process.env.CCL_TEST_CLAUDE,
    settingSources: [], settings,
    mcpServers: {}, strictMcpConfig: true,
  },
});
const timeout = setTimeout(() => {
  client.close();
  process.exitCode = 1;
}, 20000);
try {
  console.log(JSON.stringify(await client.supportedModels()));
} finally {
  clearTimeout(timeout);
  release?.();
  client.close();
}
