'use strict';

const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('fs');
const os = require('os');
const path = require('path');
const zlib = require('zlib');
const http = require('http');
const crypto = require('crypto');
const { spawn, spawnSync } = require('child_process');
const { platforms } = require('../bin/platforms');
const { resolveBinary } = require('../bin/wrapper');
const { install } = require('../bin/install');
const { buildPackages, releaseVersion, launcherFiles } = require('./build-npm-packages');

function fixture(t) {
  // macOS exposes /var through /private/var, and require.resolve returns the
  // canonical path. Keep the fixture and package resolution on the same root.
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'ccl-npm-test-')));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test('release packages contain one binary each and a small launcher', t => {
  const dir = fixture(t);
  const binaries = path.join(dir, 'binaries');
  fs.mkdirSync(binaries);
  platforms.forEach(p => fs.writeFileSync(path.join(binaries, p.asset + '.gz'), zlib.gzipSync(Buffer.from(p.asset))));
  const built = buildPackages({ version: 'v2.0.1', binaries, output: path.join(dir, 'packages') });
  const main = JSON.parse(fs.readFileSync(path.join(built.main, 'package.json')));
  assert.equal(main.version, '2.0.1');
  assert.equal(Object.keys(main.optionalDependencies).length, 6);
  assert.deepEqual(fs.readdirSync(path.join(built.main, 'bin')).sort(), [...launcherFiles].sort());
  // npm rewrites a "./"-prefixed bin target and warns on every publish, so the
  // generated manifest already carries npm's normalized form.
  assert.equal(main.bin.ccl, 'bin/ccl.exe');
  assert.equal(main.scripts.postinstall, 'node bin/install.js');
  // No shebang: npm's Windows shim must execute the placed binary directly.
  assert.ok(!fs.readFileSync(path.join(built.main, 'bin', 'ccl.exe'), 'utf8').startsWith('#!'));
  platforms.forEach(p => {
    const packageDir = path.join(built.output, p.os + '-' + p.arch);
    const pkg = JSON.parse(fs.readFileSync(path.join(packageDir, 'package.json')));
    assert.equal(pkg.name, p.packageName);
    assert.deepEqual(pkg.os, [p.os]);
    assert.deepEqual(pkg.cpu, [p.arch]);
    assert.equal(main.optionalDependencies[p.packageName], pkg.version);
    assert.deepEqual(fs.readdirSync(path.join(packageDir, 'bin')), [p.binary]);
    assert.equal(fs.readFileSync(path.join(packageDir, 'bin', p.binary), 'utf8'), p.asset);
  });
});

test('resolves all native packages and preserves legacy bundled installs', t => {
  const root = fixture(t);
  const bin = path.join(root, 'bin');
  fs.mkdirSync(bin);
  platforms.forEach(p => {
    const directory = path.join(root, 'node_modules', p.packageName, 'bin');
    fs.mkdirSync(directory, { recursive: true });
    const binary = path.join(directory, p.binary);
    fs.writeFileSync(binary, 'fake');
    assert.equal(resolveBinary(bin, p.os, p.arch), binary);
    const legacy = path.join(bin, p.asset);
    fs.writeFileSync(legacy, 'legacy');
    assert.equal(resolveBinary(bin, p.os, p.arch), legacy);
  });
  assert.throws(() => resolveBinary(bin, 'linux', 'ia32'), /Unsupported platform/);
  // The advice must be the reinstall command, not "uninstall first": npm
  // skipped the platform package, so installing again is all that is missing.
  assert.throws(() => resolveBinary(path.join(fixture(t), 'bin'), 'linux', 'x64'),
    /npm install -g @claudecodelaunch\/ccl@latest/);
});

test('postinstall replaces the placeholder with the native binary', t => {
  const root = fixture(t);
  const bin = path.join(root, 'bin');
  fs.mkdirSync(bin);
  const placeholder = path.join(bin, 'ccl.exe');
  fs.writeFileSync(placeholder, 'placeholder');
  assert.equal(install(bin, 'linux', 'x64'), false);
  assert.equal(fs.readFileSync(placeholder, 'utf8'), 'placeholder');
  const p = platforms.find(p => p.os === 'linux' && p.arch === 'x64');
  const directory = path.join(root, 'node_modules', p.packageName, 'bin');
  fs.mkdirSync(directory, { recursive: true });
  fs.writeFileSync(path.join(directory, p.binary), 'native');
  // A rerun (npm rebuild, reinstall) replaces an already placed binary.
  for (let i = 0; i < 2; i++) assert.equal(install(bin, 'linux', 'x64'), true);
  assert.equal(fs.readFileSync(placeholder, 'utf8'), 'native');
  if (process.platform !== 'win32') assert.equal(fs.statSync(placeholder).mode & 0o777, 0o755);
  assert.deepEqual(fs.readdirSync(bin), ['ccl.exe']);
});

test('placeholder falls back to wrapper.js through a bin symlink', { skip: process.platform === 'win32' }, t => {
  const root = fixture(t);
  const bin = path.join(root, 'lib', 'ccl', 'bin');
  fs.mkdirSync(bin, { recursive: true });
  ['ccl.exe', 'wrapper.js', 'platforms.js'].forEach(file => fs.copyFileSync(path.join(__dirname, '..', 'bin', file), path.join(bin, file)));
  const native = platforms.find(p => p.os === process.platform && p.arch === process.arch);
  assert.ok(native);
  fs.writeFileSync(path.join(bin, native.asset), '#!/bin/sh\necho "$1"\nexit 7\n', { mode: 0o755 });
  fs.mkdirSync(path.join(root, 'bin'));
  const link = path.join(root, 'bin', 'ccl');
  fs.symlinkSync(path.join('..', 'lib', 'ccl', 'bin', 'ccl.exe'), link);
  const result = spawnSync('/bin/sh', [link, 'argument with spaces'], { encoding: 'utf8' });
  assert.equal(result.status, 7, result.stderr);
  assert.equal(result.stdout.trim(), 'argument with spaces');
});

test('wrapper forwards arguments and nonzero exit status to the native child', t => {
  const root = fixture(t);
  const bin = path.join(root, 'bin');
  fs.mkdirSync(bin);
  ['wrapper.js', 'platforms.js'].forEach(file => fs.copyFileSync(path.join(__dirname, '..', 'bin', file), path.join(bin, file)));
  const native = platforms.find(p => p.os === process.platform && p.arch === process.arch);
  assert.ok(native);
  // The local Node executable stands in for a real native binary on every OS.
  // Link it where possible: a copied Homebrew node cannot find its @rpath libnode.
  if (process.platform === 'win32') fs.copyFileSync(process.execPath, path.join(bin, native.asset));
  else fs.symlinkSync(process.execPath, path.join(bin, native.asset));
  const result = spawnSync(process.execPath, [path.join(bin, 'wrapper.js'), '-e', 'console.log(process.argv[1]); process.exit(7)', 'argument with spaces'], { encoding: 'utf8' });
  assert.equal(result.status, 7, result.stderr);
  assert.equal(result.stdout.trim(), 'argument with spaces');
});

test('rejects invalid release versions and preserves legacy npm semver', () => {
  assert.equal(releaseVersion('v1.2.3.4'), '1.2.3-4');
  assert.throws(() => releaseVersion('dev'), /Expected/);
});

function npmCLI() {
  for (const dir of process.env.PATH.split(path.delimiter)) {
    const candidate = path.join(dir, process.platform === 'win32' ? 'npm.cmd' : 'npm');
    if (!fs.existsSync(candidate)) continue;
    const cli = process.platform === 'win32' ? path.join(dir, 'node_modules', 'npm', 'bin', 'npm-cli.js') : fs.realpathSync(candidate);
    if (fs.existsSync(cli)) return cli;
  }
  throw new Error('npm CLI not found');
}

test('npm installs exactly one native tarball from platform optional dependencies', { timeout: 45000 }, async t => {
  const root = fixture(t);
  const native = platforms.find(p => p.os === process.platform && p.arch === process.arch);
  const env = { ...process.env };
  for (const key of Object.keys(env)) {
    if (key.toLowerCase().startsWith('npm_config_') || ['NPM_TOKEN', 'NODE_AUTH_TOKEN', 'NPM_AUTH_TOKEN'].includes(key)) delete env[key];
  }
  const home = path.join(root, 'home');
  fs.mkdirSync(home);
  for (const file of ['user.npmrc', 'global.npmrc']) fs.writeFileSync(path.join(root, file), '');
  Object.assign(env, { HOME: home, USERPROFILE: home, NPM_CONFIG_USERCONFIG: path.join(root, 'user.npmrc'), NPM_CONFIG_GLOBALCONFIG: path.join(root, 'global.npmrc') });
  const binaries = path.join(root, 'binaries');
  fs.mkdirSync(binaries);
  platforms.forEach(p => fs.writeFileSync(path.join(binaries, p.asset + '.gz'), zlib.gzipSync(Buffer.from(p.asset))));
  const built = buildPackages({ version: 'v2.0.1', binaries, output: path.join(root, 'packages') });
  const cache = path.join(root, 'npm-cache');
  const cli = npmCLI();
  const packages = new Map();
  for (const directory of [...platforms.map(p => p.os + '-' + p.arch), 'main']) {
    const cwd = path.join(built.output, directory);
    const pack = spawnSync(process.execPath, [cli, 'pack', '--json', '--ignore-scripts', '--cache', cache], { cwd, env, encoding: 'utf8' });
    assert.equal(pack.status, 0, pack.stderr);
    const info = JSON.parse(pack.stdout)[0];
    const manifest = JSON.parse(fs.readFileSync(path.join(cwd, 'package.json')));
    packages.set(manifest.name, { manifest, data: fs.readFileSync(path.join(cwd, info.filename)), filename: info.filename });
  }
  const downloaded = [];
  let origin;
  let platformUnavailable = false;
  const server = http.createServer((req, res) => {
    const resource = decodeURIComponent(req.url.split('?')[0].slice(1));
    if (platformUnavailable && resource === native.packageName) {
      res.writeHead(404); res.end('{}'); return;
    }
    if (resource.startsWith('tarballs/')) {
      const pkg = [...packages.values()].find(p => p.filename === resource.slice('tarballs/'.length));
      if (!pkg) { res.writeHead(404); res.end(); return; }
      downloaded.push(pkg.manifest.name);
      res.end(pkg.data);
      return;
    }
    const pkg = packages.get(resource);
    if (!pkg) { res.writeHead(404); res.end('{}'); return; }
    const dist = { tarball: origin + '/tarballs/' + pkg.filename,
      shasum: crypto.createHash('sha1').update(pkg.data).digest('hex'),
      integrity: 'sha512-' + crypto.createHash('sha512').update(pkg.data).digest('base64') };
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ name: resource, 'dist-tags': { latest: built.version }, versions: { [built.version]: { ...pkg.manifest, dist } } }));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  origin = 'http://127.0.0.1:' + server.address().port;
  t.after(() => server.close());
  const installAt = directory => new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [cli, 'install', '@claudecodelaunch/ccl@' + built.version, '--prefix', path.join(root, directory), '--registry', origin,
      '--include=optional', '--prefer-online', '--noproxy=127.0.0.1', '--no-audit', '--no-fund', '--cache', path.join(root, directory + '-cache')], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    child.stdout.on('data', data => { output += data; });
    child.stderr.on('data', data => { output += data; });
    child.on('error', reject);
    child.on('close', code => resolve({ code, output }));
  });
  const result = await installAt('install');
  assert.equal(result.code, 0, result.output);
  assert.deepEqual(downloaded.sort(), ['@claudecodelaunch/ccl', native.packageName].sort());
  const launcherBin = path.join(root, 'install', 'node_modules', '@claudecodelaunch', 'ccl', 'bin');
  assert.equal(fs.readFileSync(resolveBinary(launcherBin, process.platform, process.arch), 'utf8'), native.asset);
  // postinstall put the native binary behind the installed command itself.
  assert.equal(fs.readFileSync(path.join(launcherBin, 'ccl.exe'), 'utf8'), native.asset);

  // Reproduce the real release race with a fresh prefix/cache: npm skips the
  // missing optional package, then our postinstall makes the install fail.
  platformUnavailable = true;
  const failed = await installAt('missing-platform');
  assert.notEqual(failed.code, 0, failed.output);
  assert.match(failed.output, /not installed|postinstall|install\.js/);
});

test('postinstall fails the install when the platform package is missing', t => {
  const root = fixture(t);
  const bin = path.join(root, 'bin');
  fs.mkdirSync(bin);
  fs.writeFileSync(path.join(bin, 'ccl.exe'), 'placeholder');
  fs.copyFileSync(path.join(__dirname, '..', 'bin', 'install.js'), path.join(bin, 'install.js'));
  fs.copyFileSync(path.join(__dirname, '..', 'bin', 'wrapper.js'), path.join(bin, 'wrapper.js'));
  fs.copyFileSync(path.join(__dirname, '..', 'bin', 'platforms.js'), path.join(bin, 'platforms.js'));
  // A zero exit code here would let npm finish an install that has no binary.
  const result = spawnSync(process.execPath, [path.join(bin, 'install.js')], { encoding: 'utf8' });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /npm install -g @claudecodelaunch\/ccl@/);
  assert.equal(fs.readFileSync(path.join(bin, 'ccl.exe'), 'utf8'), 'placeholder');
});

test('publishing waits until every platform package resolves', async () => {
  const { waitUntilInstallable } = require('./publish-npm-packages');
  const probes = [];
  const sleeps = [];
  const logs = [];
  await waitUntilInstallable('@claudecodelaunch/ccl-darwin-arm64@2.0.1', '@claudecodelaunch/ccl-darwin-arm64', '2.0.1', {
    probe: () => { probes.push(Date.now()); return probes.length >= 3; },
    sleep: ms => { sleeps.push(ms); return Promise.resolve(); },
    log: message => logs.push(message),
  });
  assert.equal(probes.length, 3);
  assert.deepEqual(sleeps, [1000, 2000]);
  assert.ok(logs.some(line => line.includes('[pending]') && line.includes('probe returned not ready')));
  assert.ok(logs.some(line => line.includes('[ready]') && line.includes('attempt=3') && line.includes('elapsed=')));
});

test('publishing gives up when a package never resolves without busy looping', async () => {
  const { waitUntilInstallable } = require('./publish-npm-packages');
  let probes = 0;
  await assert.rejects(
    waitUntilInstallable('@claudecodelaunch/ccl-linux-x64@2.0.1', '@claudecodelaunch/ccl-linux-x64', '2.0.1', {
      probe: () => { probes++; return false; },
      sleep: () => Promise.resolve(),
      now: () => 0,
      timeout: 3000,
    }),
    /still cannot resolve it/);
  assert.equal(probes, 3);
});

// Model independently propagated registry views: the full document can contain
// a version while npm's install-v1 view or the archive still does not.
function registryProbe(options = {}) {
  const fetched = [];
  const data = Buffer.from('complete tarball');
  const fetch = async (input, init) => {
    const url = new URL(input);
    fetched.push({ path: url.pathname, accept: init.headers.accept });
    if (url.pathname.endsWith('.tgz')) {
      return new Response(options.corrupt ? 'truncated' : data, { status: options.tarballStatus || 200 });
    }
    const name = decodeURIComponent(url.pathname.slice(1));
    const manifest = { name, version: '2.0.1', dist: {
      tarball: 'https://registry.npmjs.org/tarballs/' + encodeURIComponent(name) + '.tgz',
      integrity: 'sha512-' + crypto.createHash('sha512').update(data).digest('base64'),
    } };
    const missing = options.missingInstallView && init.headers.accept.includes('install-v1');
    return new Response(JSON.stringify({ name, 'dist-tags': { latest: options.latest || '2.0.1' }, versions: missing ? {} : { '2.0.1': manifest } }));
  };
  return { fetch, fetched };
}

test('readiness verifies install metadata, actual archive, integrity and latest', async () => {
  const { installable } = require('./publish-npm-packages');
  const name = platforms[0].packageName;
  for (const [fault, error] of [
    [{ missingInstallView: true }, /install-v1 metadata failed after .*missing from.*HTTP=200/],
    [{ tarballStatus: 404 }, /tarball download\/SHA512 failed after .*tarball: HTTP 404/],
    [{ corrupt: true }, /tarball download\/SHA512 failed after .*integrity mismatch/],
    [{ latest: '2.0.0' }, /latest does not point/],
  ]) {
    await assert.rejects(installable(name, '2.0.1', { fetch: registryProbe(fault).fetch, latest: true }), error);
  }
  const registry = registryProbe();
  assert.equal(await installable(name, '2.0.1', { fetch: registry.fetch, latest: true }), true);
  assert.equal(registry.fetched.length, 3);
  assert.deepEqual(registry.fetched.slice(0, 2).map(r => r.accept), ['application/json', 'application/vnd.npm.install-v1+json']);
  assert.match(registry.fetched[2].path, /\.tgz$/);
});

test('launcher is never published while any platform archive is unavailable', async t => {
  const { publishPackages } = require('./publish-npm-packages');
  const root = fixture(t);
  const optionalDependencies = {};
  for (const p of platforms) {
    const dir = path.join(root, p.os + '-' + p.arch);
    fs.mkdirSync(dir);
    fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify({ name: p.packageName, version: '2.0.1' }));
    optionalDependencies[p.packageName] = '2.0.1';
  }
  fs.mkdirSync(path.join(root, 'main'));
  fs.writeFileSync(path.join(root, 'main', 'package.json'), JSON.stringify({ name: '@claudecodelaunch/ccl', version: '2.0.1', optionalDependencies }));
  const published = [];
  await assert.rejects(publishPackages(root, {
    published: () => false,
    publish: dir => published.push(path.basename(dir)),
    fetch: registryProbe({ tarballStatus: 404 }).fetch,
    timeout: 0,
  }), /tarball: HTTP 404/);
  assert.deepEqual(published, platforms.map(p => p.os + '-' + p.arch));
  assert.ok(!published.includes('main'));

  published.length = 0;
  const registry = registryProbe();
  await publishPackages(root, {
    published: () => false,
    publish: dir => {
      if (path.basename(dir) === 'main') assert.equal(registry.fetched.filter(r => r.path.endsWith('.tgz')).length, platforms.length);
      published.push(path.basename(dir));
    },
    fetch: registry.fetch,
    timeout: 1000,
  });
  assert.equal(published.at(-1), 'main');
  assert.equal(registry.fetched.filter(r => r.path.endsWith('.tgz')).length, platforms.length + 1);
});
