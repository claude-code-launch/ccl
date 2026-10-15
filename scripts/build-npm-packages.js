#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const zlib = require('zlib');
const { platforms } = require('../bin/platforms');

function releaseVersion(value) {
  const match = /^v?(\d+\.\d+\.\d+)(?:\.(\d+))?$/.exec(value || '');
  if (!match) throw new Error('Expected vMAJOR.MINOR.PATCH (or a legacy fourth component)');
  return match[1] + (match[2] ? '-' + match[2] : '');
}

function buildPackages(options) {
  const root = options.root || path.resolve(__dirname, '..');
  const binaries = options.binaries || path.join(root, 'dist');
  const output = options.output || path.join(root, 'dist', 'npm');
  const version = releaseVersion(options.version);
  const sourcePackage = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
  // Validate every artifact before leaving a partially prepared release.
  const binaryData = platforms.map(platform => zlib.gunzipSync(fs.readFileSync(path.join(binaries, platform.asset + '.gz'))));
  if (binaryData.some(data => !data.length)) throw new Error('An npm release binary is empty');
  const optionalDependencies = {};
  platforms.forEach((platform, index) => {
    const directory = path.join(output, platform.os + '-' + platform.arch);
    fs.mkdirSync(path.join(directory, 'bin'), { recursive: true });
    fs.writeFileSync(path.join(directory, 'bin', platform.binary), binaryData[index], { mode: 0o755 });
    fs.writeFileSync(path.join(directory, 'package.json'), JSON.stringify({
      name: platform.packageName, version, description: 'CCL binary for ' + platform.os + '/' + platform.arch,
      os: [platform.os], cpu: [platform.arch], license: 'MIT', repository: sourcePackage.repository,
      files: ['bin/', 'LICENSE', 'THIRD_PARTY_NOTICES.md'], publishConfig: { access: 'public' },
    }, null, 2) + '\n');
    ['LICENSE', 'THIRD_PARTY_NOTICES.md'].forEach(file => fs.copyFileSync(path.join(root, file), path.join(directory, file)));
    optionalDependencies[platform.packageName] = version;
  });
  const main = path.join(output, 'main');
  fs.mkdirSync(path.join(main, 'bin'), { recursive: true });
  ['wrapper.js', 'platforms.js'].forEach(file => fs.copyFileSync(path.join(root, 'bin', file), path.join(main, 'bin', file)));
  fs.chmodSync(path.join(main, 'bin', 'wrapper.js'), 0o755);
  ['README.md', 'LICENSE', 'THIRD_PARTY_NOTICES.md'].forEach(file => fs.copyFileSync(path.join(root, file), path.join(main, file)));
  fs.writeFileSync(path.join(main, 'package.json'), JSON.stringify({
    ...sourcePackage, version, description: 'Multi-provider launcher for Claude Code', optionalDependencies,
    files: ['bin/wrapper.js', 'bin/platforms.js', 'README.md', 'LICENSE', 'THIRD_PARTY_NOTICES.md'],
  }, null, 2) + '\n');
  return { main, output, version };
}

module.exports = { buildPackages, releaseVersion };
if (require.main === module) {
  try { const built = buildPackages({ version: process.argv[2] }); console.log('Prepared npm packages for ' + built.version); }
  catch (err) { console.error(err.message); process.exit(1); }
}
