#!/usr/bin/env node
var spawn = require('child_process').spawn;
var fs = require('fs');
var path = require('path');
var platformFor = require('./platforms').platformFor;

function resolveBinary(binDir, os, arch) {
    var platform = platformFor(os, arch);
    if (!platform) throw new Error('Unsupported platform: ' + os + ' ' + arch);
    // Keep older bundled/source installations usable during the transition.
    var bundled = path.join(binDir, platform.asset);
    if (fs.existsSync(bundled)) return bundled;
    try {
        return require.resolve(platform.packageName + '/bin/' + platform.binary, { paths: [path.dirname(binDir)] });
    } catch (err) {
        throw new Error('ccl binary for ' + os + ' ' + arch + ' is not installed.\n' +
            'npm lists a package before it can be downloaded, so an install that ran\n' +
            'right after a release can miss the platform package. Installing again\n' +
            'fetches it; there is no need to uninstall first:\n' +
            '  npm install -g @claudecodelaunch/ccl@' + packageVersion(binDir));
    }
}

// packageVersion names the installed launcher, for a command that reinstalls
// exactly this release rather than whatever npm calls latest.
function packageVersion(binDir) {
    try {
        return JSON.parse(fs.readFileSync(path.join(path.dirname(binDir), 'package.json'), 'utf8')).version;
    } catch (err) {
        return 'latest';
    }
}

function run() {
    var binaryPath;
    try { binaryPath = resolveBinary(__dirname, process.platform, process.arch); }
    catch (err) { console.error(err.message); process.exit(1); }
    var child = spawn(binaryPath, process.argv.slice(2), { stdio: 'inherit' });
    child.on('error', function (err) {
        console.error('Failed to launch ccl: ' + err.message);
        process.exit(1);
    });
    child.on('close', function (code, signal) {
        var signals = require('os').constants.signals;
        process.exit(typeof code === 'number' ? code : (signals[signal] ? 128 + signals[signal] : 1));
    });
}

module.exports = { resolveBinary: resolveBinary };
if (require.main === module) run();
