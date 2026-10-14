#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { spawnSync } = require('child_process');
const { platforms } = require('../bin/platforms');

// Publish platform packages first: the launcher must never point to versions
// which have not been published. npm Trusted Publishing is configured per package.
const root = path.resolve(process.argv[2] || 'dist/npm');
for (const directory of [...platforms.map(p => p.os + '-' + p.arch), 'main']) {
  const cwd = path.join(root, directory);
  const pkg = JSON.parse(fs.readFileSync(path.join(cwd, 'package.json'), 'utf8'));
  const spec = pkg.name + '@' + pkg.version;
  const existing = spawnSync('npm', ['view', spec, 'version', '--json'], { encoding: 'utf8' });
  if (existing.status === 0 && JSON.parse(existing.stdout) === pkg.version) {
    console.log(spec + ' already exists; skipping.');
    continue;
  }
  if (existing.status !== 0 && !existing.stderr.includes('E404')) {
    process.stderr.write(existing.stderr);
    process.exit(existing.status || 1);
  }
  const publish = spawnSync('npm', ['publish', '--provenance', '--access', 'public'], { cwd, stdio: 'inherit' });
  if (publish.error) { console.error(publish.error.message); process.exit(1); }
  if (publish.status !== 0) process.exit(publish.status || 1);
}
