#!/usr/bin/env node
// Postinstall for @claudecodelaunch/ccl: places the platform binary over the
// bin/ccl.exe placeholder, so `ccl` executes natively. GUI clients such as
// Xcode start agents without a PATH containing node, where a node launcher
// would exit before answering. Without this step (--ignore-scripts, pnpm,
// --omit=optional) the placeholder falls back to wrapper.js.
var fs = require('fs');
var path = require('path');
var resolveBinary = require('./wrapper').resolveBinary;

// Link or copy beside dest, then rename over it: an interrupted install leaves
// either the placeholder or a complete binary.
function placeBinary(src, dest) {
    var staging = dest + '.install-' + process.pid;
    removeIfPresent(staging);
    try {
        try { fs.linkSync(src, staging); }
        catch (err) { fs.copyFileSync(src, staging); }
        if (process.platform !== 'win32') fs.chmodSync(staging, 493); // 0755
        fs.renameSync(staging, dest);
    } finally {
        removeIfPresent(staging);
    }
}

function removeIfPresent(file) {
    try { fs.unlinkSync(file); }
    catch (err) { if (err.code !== 'ENOENT') throw err; }
}

function install(binDir, os, arch) {
    var source;
    try { source = resolveBinary(binDir, os, arch); }
    catch (err) {
        console.error('[@claudecodelaunch/ccl postinstall] ' + err.message);
        console.error('  Without the platform package ccl cannot run natively.');
        return false;
    }
    placeBinary(source, path.join(binDir, 'ccl.exe'));
    return true;
}

module.exports = { install: install, placeBinary: placeBinary };
if (require.main === module) {
    var placed;
    try { placed = install(__dirname, process.platform, process.arch); }
    catch (err) {
        console.error('[@claudecodelaunch/ccl postinstall] Failed to place binary: ' + err.message);
        process.exitCode = 1;
    }
    // A failed postinstall must fail the install: exiting 0 here leaves a ccl
    // command that only reports a missing binary on first use.
    if (placed === false) process.exitCode = 1;
}
