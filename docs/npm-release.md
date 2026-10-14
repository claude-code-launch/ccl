# Platform-specific npm releases

The launcher package remains `@claudecodelaunch/ccl`. Its exact-version optional dependencies each contain one native binary, with npm `os` and `cpu` metadata selecting the current platform:

| Package suffix | Release asset |
| --- | --- |
| `darwin-x64` | `ccl-darwin-amd64.gz` |
| `darwin-arm64` | `ccl-darwin-arm64.gz` |
| `linux-x64` | `ccl-linux-amd64.gz` |
| `linux-arm64` | `ccl-linux-arm64.gz` |
| `win32-x64` | `ccl-win32-x64.exe.gz` |
| `win32-arm64` | `ccl-win32-arm64.exe.gz` |

Each suffix is prefixed with `@claudecodelaunch/ccl-`. The launcher contains the wrapper and documentation, with no native binaries. Older bundled installations are still recognized by the wrapper.

## First release setup

npm Trusted Publishing is configured separately for each package. Before the first release using this layout, an owner of the `@claudecodelaunch` npm scope must create/bootstrap the six packages and configure their trusted publisher for:

- GitHub owner/repository: `claude-code-launch/ccl`
- Workflow filename: `release.yml`
- The same publishing environment, if one is configured for the existing launcher package.

The existing launcher's trusted publisher remains in place. Initial package creation must use the maintainer's normal npm publishing access; this repository does not store a bootstrap token.

## Release workflow

`node scripts/build-npm-packages.js vMAJOR.MINOR.PATCH` validates all six compressed release assets under `dist/` and stages seven packages under `dist/npm/`. Legacy four-component tags become npm prerelease versions, matching the previous workflow.

`node scripts/publish-npm-packages.js dist/npm` checks each exact version, skips existing versions, publishes all six platform packages, then publishes the launcher. A failure prevents publishing a launcher whose required native package versions are unavailable. Reruns safely continue from already-published platform versions.

`node --test scripts/npm-packages.test.js` checks the package layout, supported targets, binary resolution, legacy installs, CLI argument forwarding and exit codes. Packaging checks do not publish anything or require registry credentials.
