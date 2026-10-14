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
const { buildPackages, releaseVersion } = require('./build-npm-packages');

function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'ccl-npm-test-'));
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
  assert.deepEqual(fs.readdirSync(path.join(built.main, 'bin')).sort(), ['platforms.js', 'wrapper.js']);
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
  assert.throws(() => resolveBinary(path.join(fixture(t), 'bin'), 'linux', 'x64'), /optional dependencies enabled/);
});

test('wrapper forwards arguments and nonzero exit status to the native child', t => {
  const root = fixture(t);
  const bin = path.join(root, 'bin');
  fs.mkdirSync(bin);
  ['wrapper.js', 'platforms.js'].forEach(file => fs.copyFileSync(path.join(__dirname, '..', 'bin', file), path.join(bin, file)));
  const native = platforms.find(p => p.os === process.platform && p.arch === process.arch);
  assert.ok(native);
  // The local Node executable stands in for a real native binary on every OS.
  fs.copyFileSync(process.execPath, path.join(bin, native.asset));
  fs.chmodSync(path.join(bin, native.asset), 0o755);
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
  const binaries = path.join(root, 'binaries');
  fs.mkdirSync(binaries);
  platforms.forEach(p => fs.writeFileSync(path.join(binaries, p.asset + '.gz'), zlib.gzipSync(Buffer.from(p.asset))));
  const built = buildPackages({ version: 'v2.0.1', binaries, output: path.join(root, 'packages') });
  const cache = path.join(root, 'npm-cache');
  const cli = npmCLI();
  const packages = new Map();
  for (const directory of [...platforms.map(p => p.os + '-' + p.arch), 'main']) {
    const cwd = path.join(built.output, directory);
    const pack = spawnSync(process.execPath, [cli, 'pack', '--json', '--ignore-scripts', '--cache', cache], { cwd, encoding: 'utf8' });
    assert.equal(pack.status, 0, pack.stderr);
    const info = JSON.parse(pack.stdout)[0];
    const manifest = JSON.parse(fs.readFileSync(path.join(cwd, 'package.json')));
    packages.set(manifest.name, { manifest, data: fs.readFileSync(path.join(cwd, info.filename)), filename: info.filename });
  }
  const downloaded = [];
  let origin;
  const server = http.createServer((req, res) => {
    const resource = decodeURIComponent(req.url.split('?')[0].slice(1));
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
  const result = await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [cli, 'install', '@claudecodelaunch/ccl@' + built.version, '--prefix', path.join(root, 'install'), '--registry', origin,
      '--noproxy=127.0.0.1', '--ignore-scripts', '--no-audit', '--no-fund', '--cache', path.join(root, 'install-cache')], { stdio: ['ignore', 'pipe', 'pipe'] });
    let output = '';
    child.stdout.on('data', data => { output += data; });
    child.stderr.on('data', data => { output += data; });
    child.on('error', reject);
    child.on('close', code => resolve({ code, output }));
  });
  assert.equal(result.code, 0, result.output);
  const native = platforms.find(p => p.os === process.platform && p.arch === process.arch);
  assert.deepEqual(downloaded.sort(), ['@claudecodelaunch/ccl', native.packageName].sort());
  const launcherBin = path.join(root, 'install', 'node_modules', '@claudecodelaunch', 'ccl', 'bin');
  assert.equal(fs.readFileSync(resolveBinary(launcherBin, process.platform, process.arch), 'utf8'), native.asset);
});
