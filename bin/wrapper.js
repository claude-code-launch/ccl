#!/usr/bin/env node
const { spawn } = require('child_process');
const path = require('path');

const ext = process.platform === 'win32' ? '.exe' : '';
const binaryName = `ccl-${process.platform}-${process.arch}${ext}`;
const binaryPath = path.join(__dirname, binaryName);

const args = process.argv.slice(2);
const child = spawn(binaryPath, args, { stdio: 'inherit' });

child.on('close', (code) => {
    process.exit(code);
});