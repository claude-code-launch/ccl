#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { execFile, spawnSync } = require('child_process');
const { platforms } = require('../bin/platforms');

const registry = 'https://registry.npmjs.org/';

function logPublish(message) {
  console.log('[' + new Date().toISOString() + '] ' + message);
}

function duration(ms) {
  if (ms < 1000) return Math.round(ms) + 'ms';
  if (ms < 60000) return (ms / 1000).toFixed(1) + 's';
  return Math.floor(ms / 60000) + 'm' + Math.floor(ms % 60000 / 1000) + 's';
}

// The registry serves packument metadata with `cache-control: max-age=300`
// and its CDN ignores a `no-cache` request header; a unique query parameter
// reaches the origin, so a rerun right after a publish sees that version.
let cacheBuster = 0;

function fresh(url) {
  const copy = new URL(url);
  copy.searchParams.set('ccl-check', Date.now().toString(36) + '-' + (++cacheBuster));
  return copy;
}

// Reruns skip versions that already exist instead of failing on a duplicate.
async function published(name, version, options = {}) {
  const request = options.fetch || fetch;
  const response = await request(fresh(new URL(name.replace('/', '%2f'), options.registry || registry)), {
    headers: { accept: 'application/json' }, signal: AbortSignal.timeout(15000), redirect: 'error',
  });
  if (response.status === 404) return false;
  if (!response.ok) throw new Error('Cannot check ' + name + '@' + version + ': HTTP ' + response.status);
  const body = await response.json();
  return Boolean(body.versions && typeof body.versions[version] === 'object');
}

// Async so the platform uploads overlap; output is captured per package so
// concurrent publishes do not interleave.
function npm(args, options = {}) {
  return new Promise((resolve, reject) => {
    execFile('npm', args, { encoding: 'utf8', maxBuffer: 8 << 20, ...options }, (error, stdout, stderr) => {
      if (error) {
        error.stdout = stdout;
        error.stderr = stderr;
        reject(error);
        return;
      }
      resolve({ stdout, stderr });
    });
  });
}

// npm accepts a publish minutes before it serves the version, and that delay
// differs per package. The launcher no longer waits for it: when npm skips a
// platform package it cannot resolve yet, bin/install.js downloads the same
// binary from the GitHub release, which the workflow creates first.
async function publishPackages(root, options = {}) {
  const log = options.log || logPublish;
  const started = Date.now();
  const exists = options.published || published;
  const publish = options.publish || (async cwd => {
    try {
      await npm(['publish', '--provenance', '--access', 'public'], { cwd, timeout: 180000 });
    } catch (err) {
      if (err.stderr) log('[publish] ' + cwd + ' npm output:\n' + err.stderr.trim());
      throw new Error('npm publish failed in ' + cwd + (err.message ? ': ' + err.message : ''));
    }
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
    if (await exists(pkg.name, pkg.version)) {
      log('[exists] ' + spec + ' already published; skipping');
      return;
    }
    const publishStarted = Date.now();
    await publish(pkg.cwd);
    log('[accepted] ' + spec + ' in ' + duration(Date.now() - publishStarted));
  }
  // A launcher is only published once npm has accepted every platform package
  // it names; a failed upload stops the release before the launcher.
  await Promise.all(packages.map(publishOne));
  await publishOne({ name: launcherPkg.name, version: launcherPkg.version, cwd: launcher });
  log('[complete] ' + launcherPkg.name + '@' + launcherPkg.version + ' total=' + duration(Date.now() - started));
}

if (require.main === module) {
  const version = spawnSync('npm', ['--version'], { encoding: 'utf8', timeout: 10000 });
  logPublish('[environment] node=' + process.version + ' npm=' + (version.status === 0 ? version.stdout.trim() : 'unknown'));
  publishPackages(path.resolve(process.argv[2] || 'dist/npm')).catch(err => {
    logPublish('[failed] ' + err.message);
    process.exit(1);
  });
}

module.exports = { published, publishPackages };
