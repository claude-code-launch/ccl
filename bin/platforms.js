'use strict';

// Node platform/architecture names and the corresponding release assets.
var platforms = [
  { os: 'darwin', arch: 'x64', asset: 'ccl-darwin-amd64' },
  { os: 'darwin', arch: 'arm64', asset: 'ccl-darwin-arm64' },
  { os: 'linux', arch: 'x64', asset: 'ccl-linux-amd64' },
  { os: 'linux', arch: 'arm64', asset: 'ccl-linux-arm64' },
  { os: 'win32', arch: 'x64', asset: 'ccl-win32-x64.exe' },
  { os: 'win32', arch: 'arm64', asset: 'ccl-win32-arm64.exe' },
];

platforms.forEach(function (platform) {
  platform.packageName = '@claudecodelaunch/ccl-' + platform.os + '-' + platform.arch;
  platform.binary = platform.os === 'win32' ? 'ccl.exe' : 'ccl';
});

function platformFor(os, arch) {
  return platforms.find(function (platform) { return platform.os === os && platform.arch === arch; });
}

module.exports = { platforms: platforms, platformFor: platformFor };
