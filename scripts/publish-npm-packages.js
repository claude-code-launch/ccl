#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const { spawnSync } = require('child_process');
const { platforms } = require('../bin/platforms');

const registry = 'https://registry.npmjs.org/';
const installAccept = 'application/vnd.npm.install-v1+json';
const waitTimeout = 30 * 60 * 1000;

function logPublish(message) {
  console.log('[' + new Date().toISOString() + '] ' + message);
}

function duration(ms) {
  if (ms < 1000) return Math.round(ms) + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.floor(ms % 60000 / 1000) + 's';
}

// Only public cache diagnostics, never request headers or response bodies.
function responseDetails(response) {
  const fields = ['HTTP=' + response.status];
  for (const header of ['cf-cache-status', 'age', 'last-modified']) {
    const value = response.headers.get(header);
    if (value !== null) fields.push(header + '=' + value);
  }
  return fields.join(' ');
}

// Existence and installability are different: never republish an existing
// version, but do not mistake npm view's answer for a downloadable package.
function published(name, version) {
  const result = spawnSync('npm', ['view', name + '@' + version, 'version', '--json', '--prefer-online'], {
    encoding: 'utf8', timeout: 60000,
  });
  if (result.error) throw result.error;
  if (result.status === 0) return JSON.parse(result.stdout) === version;
  if ((result.stderr || '').includes('E404')) return false;
  throw new Error('Cannot check ' + name + '@' + version + ': ' + result.stderr);
}

async function bodyBytes(response, limit) {
  const chunks = [];
  let size = 0;
  for await (const chunk of response.body) {
    size += chunk.length;
    if (size > limit) throw new Error('registry response exceeded ' + limit + ' bytes');
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

async function installable(name, version, options = {}) {
  const origin = options.registry || registry;
  const request = options.fetch || fetch;
  const log = options.log || logPublish;
  const spec = name + '@' + version;
  const metadataURL = new URL(name.replace('/', '%2f'), origin);
  const manifests = [];
  let stage;
  let started;
  let details;
  try {
    for (const accept of ['application/json', installAccept]) {
      stage = accept === installAccept ? 'install-v1 metadata' : 'full metadata';
      started = Date.now();
      details = '';
      const response = await request(metadataURL, {
        headers: { accept, 'cache-control': 'no-cache', pragma: 'no-cache' },
        signal: AbortSignal.timeout(15000), redirect: 'error',
      });
      details = responseDetails(response);
      if (!response.ok) throw new Error(name + ' metadata: HTTP ' + response.status);
      const catalog = JSON.parse((await bodyBytes(response, 8 << 20)).toString('utf8'));
      const latest = catalog['dist-tags'] && catalog['dist-tags'].latest;
      details += ' latest=' + (latest || '(absent)');
      const recorded = catalog.time && catalog.time[version];
      if (recorded) details += ' version-time=' + recorded;
      const manifest = catalog.versions && catalog.versions[version];
      if (!manifest || manifest.version !== version || manifest.name !== name || !manifest.dist) {
        throw new Error(name + '@' + version + ' missing from ' + accept + ' metadata');
      }
      if (options.latest && latest !== version) {
        throw new Error(name + ' latest does not point to ' + version);
      }
      manifests.push(manifest);
      log('[check] ' + spec + ' ' + stage + ' OK in ' + duration(Date.now() - started) + '; ' + details);
    }
    stage = 'metadata consistency';
    started = Date.now();
    details = '';
    const dist = manifests[0].dist;
    if (!dist.tarball || manifests[1].dist.tarball !== dist.tarball || manifests[1].dist.integrity !== dist.integrity) {
      throw new Error(name + ' metadata disagrees about the tarball');
    }
    const tarball = new URL(dist.tarball);
    if (tarball.origin !== metadataURL.origin) throw new Error('unexpected tarball origin for ' + name);
    stage = 'tarball download/SHA512';
    started = Date.now();
    const response = await request(tarball, {
      headers: { 'cache-control': 'no-cache' }, signal: AbortSignal.timeout(30000), redirect: 'error',
    });
    details = responseDetails(response);
    if (!response.ok) throw new Error(name + ' tarball: HTTP ' + response.status);
    // Hash the complete archive, without retaining six platform tarballs in RAM.
    const hash = crypto.createHash('sha512');
    let size = 0;
    for await (const chunk of response.body) {
      size += chunk.length;
      if (size > 128 << 20) throw new Error(name + ' tarball is too large');
      hash.update(chunk);
    }
    const want = 'sha512-' + hash.digest('base64');
    if (!size || !(dist.integrity || '').split(/\s+/).includes(want)) {
      throw new Error(name + ' tarball integrity mismatch');
    }
    log('[check] ' + spec + ' ' + stage + ' OK in ' + duration(Date.now() - started) + '; bytes=' + size + ' ' + details);
    return true;
  } catch (err) {
    const cause = err.cause && (err.cause.code || err.cause.message);
    throw new Error(stage + ' failed after ' + duration(Date.now() - started) + ': ' + err.message +
      (cause ? ' (' + cause + ')' : '') + (details ? '; ' + details : ''), { cause: err });
  }
}

// One deadline covers the whole platform set, not 30 minutes per package.
// Count scheduled sleeps as well as wall time, so a fake clock or resolved
// test promise cannot turn an unsuccessful probe into an unbounded busy loop.
async function waitForPackages(packages, options = {}) {
  const probe = options.probe || ((name, version) => installable(name, version, options));
  const sleep = options.sleep || (ms => new Promise(resolve => setTimeout(resolve, ms)));
  const now = options.now || Date.now;
  const log = options.log || logPublish;
  const timeout = options.timeout === undefined ? waitTimeout : options.timeout;
  const start = now();
  const attempts = new Map();
  let waited = 0;
  let delay = 1000;
  let pending = packages;
  let lastError;
  log('[wait] checking ' + packages.length + ' package(s); budget=' + duration(timeout));
  for (;;) {
    const remaining = [];
    for (const pkg of pending) {
      const spec = pkg.name + '@' + pkg.version;
      const attempt = (attempts.get(spec) || 0) + 1;
      attempts.set(spec, attempt);
      const checkStarted = now();
      let reason;
      try {
        if (!await probe(pkg.name, pkg.version)) {
          remaining.push(pkg);
          reason = 'probe returned not ready';
        }
      } catch (err) {
        lastError = err;
        remaining.push(pkg);
        reason = err.message;
      }
      const timing = ' attempt=' + attempt + ' elapsed=' + duration(Math.max(now() - start, waited)) +
        ' check=' + duration(now() - checkStarted);
      log((reason ? '[pending] ' : '[ready] ') + spec + timing + (reason ? '; ' + reason : ''));
      if (Math.max(now() - start, waited) >= timeout && remaining.length) break;
    }
    if (!remaining.length) {
      log('[ready] all ' + packages.length + ' package(s) verified in ' + duration(Math.max(now() - start, waited)));
      return;
    }
    const left = timeout - Math.max(now() - start, waited);
    if (left <= 0) {
      throw new Error('Registry still cannot resolve it for installation: ' +
        remaining.map(p => p.name + '@' + p.version).join(', ') + (lastError ? '; ' + lastError.message : ''));
    }
    pending = remaining;
    const pause = Math.min(delay, left);
    log('[retry] ' + pending.length + ' package(s) pending; next check in ' + duration(pause) + '; budget remaining=' + duration(left));
    await sleep(pause);
    waited += pause;
    delay = Math.min(delay * 2, 30000);
  }
}

async function waitUntilInstallable(spec, name, version, options) {
  return waitForPackages([{ name, version }], options);
}

async function publishPackages(root, options = {}) {
  const log = options.log || logPublish;
  const started = Date.now();
  const exists = options.published || published;
  const publish = options.publish || (cwd => {
    const result = spawnSync('npm', ['publish', '--provenance', '--access', 'public'], { cwd, stdio: 'inherit', timeout: 180000 });
    if (result.error) throw result.error;
    if (result.status !== 0) throw new Error('npm publish failed in ' + cwd);
  });
  const launcher = path.join(root, 'main');
  const launcherPkg = JSON.parse(fs.readFileSync(path.join(launcher, 'package.json'), 'utf8'));
  const packages = platforms.map(platform => {
    const cwd = path.join(root, platform.os + '-' + platform.arch);
    const pkg = JSON.parse(fs.readFileSync(path.join(cwd, 'package.json'), 'utf8'));
    if (pkg.name !== platform.packageName || !launcherPkg.optionalDependencies || launcherPkg.optionalDependencies[pkg.name] !== pkg.version) {
      throw new Error('launcher dependency does not match ' + platform.packageName);
    }
    return { cwd, name: pkg.name, version: pkg.version };
  });
  async function publishOne(pkg) {
    const spec = pkg.name + '@' + pkg.version;
    const checkStarted = Date.now();
    log('[exists] ' + spec + ' checking registry');
    const found = await exists(pkg.name, pkg.version);
    log('[exists] ' + spec + ' ' + (found ? 'already published; skipping upload' : 'not published') +
      ' in ' + duration(Date.now() - checkStarted));
    if (!found) {
      const publishStarted = Date.now();
      log('[publish] ' + spec + ' starting upload');
      await publish(pkg.cwd);
      log('[accepted] ' + spec + ' npm publish returned successfully in ' + duration(Date.now() - publishStarted) +
        '; installability is not yet confirmed');
    }
  }
  log('[phase] publishing ' + packages.length + ' platform packages');
  for (const pkg of packages) await publishOne(pkg);
  // Both npm metadata representations and every archive must be usable before
  // the launcher referencing those exact platform versions can be published.
  log('[phase] verifying platform availability');
  await waitForPackages(packages, options);
  log('[phase] platforms ready; publishing launcher');
  await publishOne({ name: launcherPkg.name, version: launcherPkg.version, cwd: launcher });
  log('[phase] verifying launcher availability and latest tag');
  await waitForPackages([{ name: launcherPkg.name, version: launcherPkg.version }], { ...options, latest: true });
  log('[complete] ' + launcherPkg.name + '@' + launcherPkg.version + ' installable; total=' + duration(Date.now() - started));
}

if (require.main === module) {
  const npm = spawnSync('npm', ['--version'], { encoding: 'utf8', timeout: 10000 });
  logPublish('[environment] node=' + process.version + ' npm=' + (npm.status === 0 ? npm.stdout.trim() : 'unknown'));
  publishPackages(path.resolve(process.argv[2] || 'dist/npm')).catch(err => {
    logPublish('[failed] ' + err.message);
    process.exit(1);
  });
}

module.exports = { published, installable, waitUntilInstallable, waitForPackages, publishPackages };
