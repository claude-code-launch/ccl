#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');
const { platforms } = require('../bin/platforms');

// Publish platform packages first: the launcher must never point to versions
// which have not been published. npm Trusted Publishing is configured per package.
//
// A package is not installable the moment `npm publish` returns: the registry
// accepts it, runs provenance, and only then serves its metadata. The launcher
// pins exact platform versions, and npm silently skips an optional dependency
// that cannot be resolved, so publishing the launcher while a platform package
// is still being processed installs a ccl with no binary. Wait for every
// published package to answer before moving on.
const root = path.resolve(process.argv[2] || 'dist/npm');

function published(name, version) {
  const result = spawnSync('npm', ['view', name + '@' + version, 'version', '--json'], { encoding: 'utf8' });
  if (result.status !== 0) return false;
  try { return JSON.parse(result.stdout) === version; }
  catch (err) { return false; }
}

// waitUntilInstallable polls until the registry resolves spec. The probe and
// sleep are injectable so the behaviour is testable without a registry.
async function waitUntilInstallable(spec, name, version, options) {
  const probe = (options && options.probe) || published;
  const sleep = (options && options.sleep) || (ms => new Promise(resolve => setTimeout(resolve, ms)));
  // An explicit 0 (a test) must mean "no waiting", not "use the default".
  const timeout = options && options.timeout !== undefined ? options.timeout : 15 * 60 * 1000;
  const deadline = Date.now() + timeout;
  let delay = 1000;
  for (;;) {
    if (probe(name, version)) return;
    if (Date.now() > deadline) {
      throw new Error(spec + ' was published but the registry still cannot resolve it after 15 minutes');
    }
    console.log(spec + ' is not in the registry yet; waiting ' + Math.round(delay / 1000) + 's');
    await sleep(delay);
    delay = Math.min(delay * 2, 30000);
  }
}

async function main() {
  const launcher = path.join(root, 'main');
  const launcherPkg = JSON.parse(fs.readFileSync(path.join(launcher, 'package.json'), 'utf8'));
  const platformDirs = platforms.map(p => ({ directory: p.os + '-' + p.arch, name: p.packageName }));

  for (const { directory, name } of platformDirs) {
    const cwd = path.join(root, directory);
    const pkg = JSON.parse(fs.readFileSync(path.join(cwd, 'package.json'), 'utf8'));
    const spec = name + '@' + pkg.version;
    if (published(name, pkg.version)) {
      console.log(spec + ' already exists; skipping.');
      continue;
    }
    const publish = spawnSync('npm', ['publish', '--provenance', '--access', 'public'], { cwd, stdio: 'inherit' });
    if (publish.error) throw publish.error;
    if (publish.status !== 0) throw new Error('npm publish failed for ' + spec);
  }
  // Every platform package the launcher depends on must resolve first.
  for (const { name } of platformDirs) {
    const version = launcherPkg.optionalDependencies[name];
    if (!version) throw new Error('the launcher lists no ' + name + ' dependency');
    await waitUntilInstallable(name + '@' + version, name, version);
  }

  const launcherSpec = launcherPkg.name + '@' + launcherPkg.version;
  if (published(launcherPkg.name, launcherPkg.version)) {
    console.log(launcherSpec + ' already exists; skipping.');
  } else {
    const publish = spawnSync('npm', ['publish', '--provenance', '--access', 'public'], { cwd: launcher, stdio: 'inherit' });
    if (publish.error) throw publish.error;
    if (publish.status !== 0) throw new Error('npm publish failed for ' + launcherSpec);
  }
  // The version published is the one `npm install -g …@latest` must install.
  await waitUntilInstallable(launcherSpec, launcherPkg.name, launcherPkg.version);
}

if (require.main === module) {
  main().catch(err => { console.error(err.message); process.exit(1); });
}

module.exports = { published, waitUntilInstallable };
