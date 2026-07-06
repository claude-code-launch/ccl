#!/usr/bin/env node
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');

const archMap = {
    darwin: { x64: 'amd64', arm64: 'arm64' },
    linux: { x64: 'amd64', arm64: 'arm64' },
    win32: { x64: 'x64', arm64: 'arm64' },
};

const platformArch = archMap[process.platform]?.[process.arch];
if (!platformArch) {
    console.error(`Unsupported platform: ${process.platform} ${process.arch}`);
    process.exit(1);
}

const ext = process.platform === 'win32' ? '.exe' : '';
const binaryName = `ccl-${process.platform}-${platformArch}${ext}`;
const binaryPath = path.join(__dirname, binaryName);

if (!fs.existsSync(binaryPath)) {
    console.error(`ccl binary not found: ${binaryPath}`);
    console.error('Reinstall @claudecodelaunch/ccl or download a release binary from GitHub.');
    process.exit(1);
}

const args = process.argv.slice(2);
const child = spawn(binaryPath, args, { stdio: 'inherit' });

child.on('error', (err) => {
    console.error(`Failed to launch ccl: ${err.message}`);
    process.exit(1);
});

child.on('close', (code) => {
    process.exit(code ?? 1);
});
