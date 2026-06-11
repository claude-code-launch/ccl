const fs = require('fs');
const path = require('path');
const https = require('https');
const { execSync } = require('child_process');

const OWNER = "claude-code-launch";
const REPO = "ccl";
const BINARY_NAME = "ccl";

// 平台和架构映射
const platformMap = {
    'darwin': 'darwin',
    'linux': 'linux',
    'win32': 'windows'
};
const archMap = {
    'x64': 'amd64',
    'arm64': 'arm64'
};

const os = platformMap[process.platform];
const arch = archMap[process.arch];

if (!os || !arch) {
    console.error(`❌ Unsupported platform/architecture: ${process.platform}/${process.arch}`);
    process.exit(1);
}

// 动态获取最新 Release tag
function getLatestReleaseTag(callback) {
    const options = {
        hostname: 'api.github.com',
        path: `/repos/${OWNER}/${REPO}/releases/latest`,
        headers: { 'User-Agent': 'node-install-script' }
    };

    https.get(options, (res) => {
        let data = '';
        res.on('data', chunk => data += chunk);
        res.on('end', () => {
            const release = JSON.parse(data);
            callback(release.tag_name);
        });
    }).on('error', (err) => {
        console.error(`❌ Failed to fetch latest release: ${err.message}`);
        process.exit(1);
    });
}

getLatestReleaseTag((VERSION) => {
    const ext = os === 'windows' ? '.exe' : '';
    const filename = `${BINARY_NAME}_${os}_${arch}${os === 'windows' ? '.zip' : '.tar.gz'}`;
    const url = `https://github.com/${OWNER}/${REPO}/releases/download/${VERSION}/${filename}`;

    const binDir = path.join(__dirname, '../bin');
    if (!fs.existsSync(binDir)) fs.mkdirSync(binDir);
    const tarPath = path.join(binDir, filename);

    console.log(`📥 Downloading native binary from ${url}...`);

    const file = fs.createWriteStream(tarPath);
    https.get(url, (response) => {
        const stream = (response.statusCode === 302 || response.statusCode === 301)
            ? https.get(response.headers.location, (redirectResp) => redirectResp.pipe(file))
            : response.pipe(file);

        file.on('finish', () => {
            file.close();
            console.log('📦 Extracting binary...');

            if (os === 'windows') {
                execSync(`tar -xf "${tarPath}" -C "${binDir}"`);
            } else {
                execSync(`tar -xzf "${tarPath}" -C "${binDir}"`);
                fs.chmodSync(path.join(binDir, BINARY_NAME), '755');
            }

            fs.unlinkSync(tarPath);
            console.log('🎉 Installation complete!');
        });
    }).on('error', (err) => {
        fs.unlinkSync(tarPath);
        console.error(`❌ Download failed: ${err.message}`);
        process.exit(1);
    });
});
