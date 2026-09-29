#!/usr/bin/env node
var spawn = require('child_process').spawn;
var fs = require('fs');
var path = require('path');

var archMap = {
    darwin: { x64: 'amd64', arm64: 'arm64' },
    linux: { x64: 'amd64', arm64: 'arm64' },
    win32: { x64: 'x64', arm64: 'arm64' },
};

var platformArch = archMap[process.platform] && archMap[process.platform][process.arch];
if (!platformArch) {
    console.error('Unsupported platform: ' + process.platform + ' ' + process.arch);
    process.exit(1);
}

var ext = process.platform === 'win32' ? '.exe' : '';
var binaryName = 'ccl-' + process.platform + '-' + platformArch + ext;
var binaryPath = path.join(__dirname, binaryName);

if (!fs.existsSync(binaryPath)) {
    console.error('ccl binary not found: ' + binaryPath);
    console.error('Reinstall @claudecodelaunch/ccl or download a release binary from GitHub.');
    process.exit(1);
}

var args = process.argv.slice(2);
var child = spawn(binaryPath, args, { stdio: 'inherit' });

child.on('error', function (err) {
    console.error('Failed to launch ccl: ' + err.message);
    process.exit(1);
});

child.on('close', function (code) {
    process.exit(typeof code === 'number' ? code : 1);
});
