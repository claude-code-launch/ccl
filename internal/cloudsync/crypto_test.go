package cloudsync

import (
	"bytes"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealAndOpenCompressedRoundTrip(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, 32)
	plain := []byte(strings.Repeat("ccl snapshot ", 512))

	sealed, err := sealCompressed(key, plain)
	if err != nil {
		t.Fatalf("sealCompressed() error = %v", err)
	}
	if !bytes.HasPrefix(sealed, envelopeMagic) {
		t.Fatal("sealed envelope has no magic header")
	}
	// The ciphertext is authenticated: a flipped bit anywhere is rejected.
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := openCompressed(key, tampered); err == nil ||
		!strings.Contains(err.Error(), "wrong passphrase") {
		t.Fatalf("tampered envelope error = %v", err)
	}
	if _, err := openCompressed(bytes.Repeat([]byte{0x2b}, 32), sealed); err == nil {
		t.Fatal("another key opened the envelope")
	}

	opened, err := openCompressed(key, sealed)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("openCompressed() = %d bytes, %v", len(opened), err)
	}

	for name, value := range map[string]struct {
		key       []byte
		encrypted []byte
		want      string
	}{
		"short key":      {key: key[:16], encrypted: sealed, want: "invalid encryption key"},
		"short envelope": {key: key, encrypted: envelopeMagic, want: "invalid encrypted sync envelope"},
		"wrong magic":    {key: key, encrypted: append([]byte("CCLSYNC2"), sealed[8:]...), want: "invalid encrypted sync envelope"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := openCompressed(value.key, value.encrypted); err == nil ||
				!strings.Contains(err.Error(), value.want) {
				t.Fatalf("openCompressed() error = %v, want %q", err, value.want)
			}
		})
	}
	if _, err := sealCompressed(key[:16], plain); err == nil ||
		!strings.Contains(err.Error(), "invalid encryption key") {
		t.Fatalf("sealCompressed() with a short key = %v", err)
	}
}

// TestOpenCompressedRejectsAnExpandedEnvelope pins the decompression-bomb
// guard: the limit applies to the decompressed size, not the envelope.
func TestOpenCompressedRejectsAnExpandedEnvelope(t *testing.T) {
	key := bytes.Repeat([]byte{0x2a}, 32)
	// Highly compressible data just over the limit expands to a tiny envelope.
	sealed, err := sealCompressed(key, bytes.Repeat([]byte{0}, maxEncryptedSize+1))
	if err != nil {
		t.Fatalf("sealCompressed() error = %v", err)
	}
	if len(sealed) > 1<<20 {
		t.Fatalf("fixture envelope = %d bytes; expected it to compress", len(sealed))
	}
	if _, err := openCompressed(key, sealed); err == nil ||
		!strings.Contains(err.Error(), "exceeds safety limit") {
		t.Fatalf("oversized payload error = %v", err)
	}
}

func TestDeriveKeyValidations(t *testing.T) {
	profile, err := newPassphraseProfile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deriveKey("too-short", profile); err == nil ||
		!strings.Contains(err.Error(), "at least 12 characters") {
		t.Fatalf("short passphrase error = %v", err)
	}
	key, err := deriveKey("correct horse battery staple", profile)
	if err != nil || len(key) != 32 {
		t.Fatalf("deriveKey() = %d bytes, %v", len(key), err)
	}
	// The same passphrase and salt must derive the same key.
	again, err := deriveKey("correct horse battery staple", profile)
	if err != nil || !bytes.Equal(key, again) {
		t.Fatalf("deriveKey() is not deterministic: %v", err)
	}

	masterKeyProfile, _, err := newMasterKeyProfile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deriveKey("correct horse battery staple", masterKeyProfile); err == nil ||
		!strings.Contains(err.Error(), "random recovery key") {
		t.Fatalf("master-key profile error = %v", err)
	}
	for name, broken := range map[string]struct {
		profile remoteProfile
		want    string
	}{
		"wrong version": {profile: remoteProfile{Version: 99, ID: profile.ID, KDF: kdfScrypt, N: 1 << 15, R: 8, P: 1, Salt: profile.Salt}, want: "unsupported or unsafe cloud sync profile"},
		"weak kdf":      {profile: remoteProfile{Version: formatVersion, ID: profile.ID, KDF: kdfScrypt, N: 1 << 10, R: 8, P: 1, Salt: profile.Salt}, want: "unsupported or unsafe cloud sync profile"},
		"unknown kdf":   {profile: remoteProfile{Version: formatVersion, ID: profile.ID, KDF: "pbkdf2", N: 1 << 15, R: 8, P: 1, Salt: profile.Salt}, want: "unsupported or unsafe cloud sync profile"},
		"bad salt":      {profile: remoteProfile{Version: formatVersion, ID: profile.ID, KDF: kdfScrypt, N: 1 << 15, R: 8, P: 1, Salt: "!!!"}, want: "invalid cloud sync salt"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := deriveKey("correct horse battery staple", broken.profile); err == nil ||
				!strings.Contains(err.Error(), broken.want) {
				t.Fatalf("deriveKey() error = %v, want %q", err, broken.want)
			}
		})
	}
	// A short salt decodes but is not the required 16 bytes.
	short := profile
	short.Salt = "AAAA"
	if _, err := deriveKey("correct horse battery staple", short); err == nil ||
		!strings.Contains(err.Error(), "invalid cloud sync salt") {
		t.Fatalf("short salt error = %v", err)
	}
}

func TestValidateRemoteProfileAcceptsBothKeyTypes(t *testing.T) {
	passphraseProfile, err := newPassphraseProfile()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRemoteProfile(passphraseProfile); err != nil {
		t.Fatalf("passphrase profile rejected: %v", err)
	}
	generated := passphraseProfile
	generated.PairingPublicKey = testPairingPublicKey(t)
	if err := validateRemoteProfile(generated); err != nil {
		t.Fatalf("profile with a pairing key rejected: %v", err)
	}
	masterKeyProfile, _, err := newMasterKeyProfile()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRemoteProfile(masterKeyProfile); err != nil {
		t.Fatalf("master-key profile rejected: %v", err)
	}

	for name, broken := range map[string]remoteProfile{
		"wrong version":    {Version: 99, ID: passphraseProfile.ID, KDF: kdfScrypt},
		"short id":         {Version: formatVersion, ID: "abc", KDF: kdfScrypt},
		"non-hex id":       {Version: formatVersion, ID: strings.Repeat("z", 32), KDF: kdfScrypt},
		"weak kdf":         {Version: formatVersion, ID: passphraseProfile.ID, KDF: kdfScrypt, N: 2, R: 8, P: 1, Salt: passphraseProfile.Salt},
		"unknown kdf":      {Version: formatVersion, ID: passphraseProfile.ID, KDF: "pbkdf2"},
		"master key extra": {Version: formatVersion, ID: passphraseProfile.ID, KDF: kdfMasterKey, Salt: passphraseProfile.Salt},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRemoteProfile(broken); err == nil {
				t.Fatalf("validateRemoteProfile(%+v) accepted an invalid profile", broken)
			}
		})
	}
	badSalt := passphraseProfile
	badSalt.Salt = "!!!"
	if err := validateRemoteProfile(badSalt); err == nil ||
		!strings.Contains(err.Error(), "invalid cloud sync salt") {
		t.Fatalf("bad salt error = %v", err)
	}
	badPairing := passphraseProfile
	badPairing.PairingPublicKey = "not-a-key"
	if err := validateRemoteProfile(badPairing); err == nil {
		t.Fatal("an unusable pairing key was accepted")
	}
}

func TestEncodeRecoveryKeyRejectsUnusableInputs(t *testing.T) {
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encodeRecoveryKey("not-hex", bytes.Repeat([]byte{1}, 32)); err == nil ||
		!strings.Contains(err.Error(), "invalid profile or encryption key") {
		t.Fatalf("bad profile id error = %v", err)
	}
	if _, err := encodeRecoveryKey(profileID, bytes.Repeat([]byte{1}, 16)); err == nil ||
		!strings.Contains(err.Error(), "invalid profile or encryption key") {
		t.Fatalf("short key error = %v", err)
	}
}

func TestDecodeRecoveryKeyRejectsMalformedValues(t *testing.T) {
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encodeRecoveryKey(profileID, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	groups := strings.Split(strings.TrimPrefix(encoded, recoveryKeyPrefix+"-"), "-")
	if len(groups) == 0 {
		t.Fatalf("encoded key has no groups: %q", encoded)
	}

	for name, value := range map[string]string{
		"empty":           "",
		"wrong prefix":    "CCL2-" + strings.Join(groups, "-"),
		"not base32":      recoveryKeyPrefix + "-!!!" + strings.Join(groups[:1], ""),
		"truncated":       recoveryKeyPrefix + "-" + strings.Join(groups[:len(groups)-1], "-"),
		"payload version": recoveryKeyPrefix + "-" + strings.Join(groups, "-") + "A",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := decodeRecoveryKey(value); err == nil {
				t.Fatalf("decodeRecoveryKey(%q) accepted a malformed value", value)
			}
		})
	}
	// A single flipped character inside the payload must fail the checksum.
	// (Position 0 would fail earlier: it is the payload version byte.)
	tampered := []byte(strings.Join(groups, ""))
	if tampered[10] == 'A' {
		tampered[10] = 'B'
	} else {
		tampered[10] = 'A'
	}
	if _, _, err := decodeRecoveryKey(encoded); err != nil {
		t.Fatalf("the untampered key was rejected: %v", err)
	}
	broken := recoveryKeyPrefix + "-" + string(tampered)
	if _, _, err := decodeRecoveryKey(broken); err == nil ||
		!strings.Contains(err.Error(), "checksum") {
		t.Fatalf("tampered key error = %v", err)
	}
	// Lowercase and spaced input is the same key.
	reformatted := strings.ToLower(recoveryKeyPrefix + " " + strings.Join(groups, " "))
	gotID, gotKey, err := decodeRecoveryKey(reformatted)
	if err != nil || gotID != profileID || !bytes.Equal(gotKey, bytes.Repeat([]byte{7}, 32)) {
		t.Fatalf("reformatted key = %s, %d bytes, %v", gotID, len(gotKey), err)
	}
}

// testPairingPublicKey asks the package's own pairing code for a valid key, so
// the profile validator is exercised against something it must accept.
func testPairingPublicKey(t *testing.T) string {
	t.Helper()
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	public, err := pairingPublicKey(bytes.Repeat([]byte{9}, 32), profileID)
	if err != nil {
		t.Fatalf("pairingPublicKey: %v", err)
	}
	return public
}

// TestRecoveryKeyRoundTripsThroughAProfile ties the encoder to the decoder with
// the identifiers the real login produces.
func TestRecoveryKeyRoundTripsThroughAProfile(t *testing.T) {
	profile, _, err := newMasterKeyProfile()
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.ID) != 32 {
		t.Fatalf("profile id = %q", profile.ID)
	}
	if _, err := hex.DecodeString(profile.ID); err != nil {
		t.Fatalf("profile id is not hex: %v", err)
	}
}

// TestSnapshotFileValidationRejectsUnsupportedPaths pins the archive whitelist:
// only config.yaml and auth/*.json may travel, exactly once each.
func TestSnapshotFileValidationRejectsUnsupportedPaths(t *testing.T) {
	valid := []snapshotFile{
		{Path: "config.yaml"},
		{Path: "auth/zed-42.json"},
		{Path: "auth/CREDENTIAL.JSON"},
	}
	if err := validateSnapshotFiles(valid); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	if got := hashSnapshotFiles(valid); got == "" || len(got) != 64 {
		t.Fatalf("snapshot hash = %q", got)
	}

	for name, files := range map[string][]snapshotFile{
		"empty":           {{Path: ""}},
		"other file":      {{Path: "settings.json"}},
		"nested auth":     {{Path: "auth/nested/credential.json"}},
		"auth non-json":   {{Path: "auth/notes.txt"}},
		"absolute":        {{Path: "/etc/passwd"}},
		"traversal":       {{Path: "../config.yaml"}},
		"unclean":         {{Path: "auth/./credential.json"}},
		"backslash":       {{Path: `auth\credential.json`}},
		"duplicate":       {{Path: "config.yaml"}, {Path: "config.yaml"}},
		"duplicate clean": {{Path: "auth/a.json"}, {Path: "auth/a.json"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateSnapshotFiles(files); err == nil {
				t.Fatalf("validateSnapshotFiles(%+v) accepted an unsupported snapshot", files)
			}
		})
	}
}

// TestReadRegularFileEnforcesItsLimitAndType covers the two guards every cloud
// file read goes through.
func TestReadRegularFileEnforcesItsLimitAndType(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.json")
	if err := writeAtomic(small, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readRegularFile(small, 1<<10); err != nil || string(data) != "{}" {
		t.Fatalf("readRegularFile() = %q, %v", data, err)
	}
	if _, err := readRegularFile(small, 1); err == nil ||
		!strings.Contains(err.Error(), "exceeds safety limit") {
		t.Fatalf("oversized read error = %v", err)
	}
	if _, err := readRegularFile(filepath.Join(dir, "missing.json"), 1<<10); err == nil {
		t.Fatal("reading a missing file succeeded")
	}

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(small, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFile(link, 1<<10); err == nil ||
		!strings.Contains(err.Error(), "non-regular file") {
		t.Fatalf("symlinked read error = %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularFile(filepath.Join(dir, "dir.json"), 1<<10); err == nil {
		t.Fatal("reading a directory succeeded")
	}
}

// TestWriteAtomicRefusesUnsafeTargets pins the write-side guard and the mode it
// leaves behind.
func TestWriteAtomicRefusesUnsafeTargets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "file.json")
	if err := writeAtomic(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writeAtomic() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("written mode = %v, %v", info.Mode().Perm(), err)
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || parent.Mode().Perm() != 0o700 {
		t.Fatalf("parent mode = %v, %v", parent.Mode().Perm(), err)
	}

	// A symlink where the parent directory belongs is refused, not followed.
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(link, "file.json"), []byte("{}"), 0o600); err == nil ||
		!strings.Contains(err.Error(), "non-directory path") {
		t.Fatalf("writeAtomic() through a symlinked parent = %v", err)
	}
	if err := writeAtomic(dir, []byte("{}"), 0o600); err == nil {
		t.Fatal("writeAtomic() overwrote a directory")
	}
}

// TestJSONAtomicRequiresEncodableValues pins that a marshalling failure leaves
// no partial file behind.
func TestJSONAtomicRequiresEncodableValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "value.json")
	if err := writeJSONAtomic(path, make(chan int), 0o600); err == nil {
		t.Fatal("an unencodable value was written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("a failed write left %s behind (err=%v)", path, err)
	}
}

// TestCollectLocalFilesRequiresConfigurationOrCredentials covers the empty case
// that stops a push before it uploads anything.
func TestCollectLocalFilesRequiresConfigurationOrCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, _, err := collectLocalFiles(); err != ErrNoLocalData {
		t.Fatalf("collectLocalFiles() on an empty home = %v, want ErrNoLocalData", err)
	}

	if err := os.MkdirAll(filepath.Join(home, ".ccl", "auth"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(home, ".ccl", "config.yaml"), []byte("active_provider: test\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	// A directory, a non-JSON file and a backslash name are all skipped: only
	// the config travels, so the snapshot is still usable.
	auth := filepath.Join(home, ".ccl", "auth")
	if err := os.Mkdir(filepath.Join(auth, "nested.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auth, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, hash, err := collectLocalFiles()
	if err != nil {
		t.Fatalf("collectLocalFiles() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != "config.yaml" || hash == "" {
		t.Fatalf("collected %+v (hash %q)", files, hash)
	}

	// A symlinked credential is refused rather than archived.
	real := filepath.Join(home, "real-credential.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(auth, "linked.json")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectLocalFiles(); err == nil ||
		!strings.Contains(err.Error(), "non-regular file") {
		t.Fatalf("collectLocalFiles() with a symlinked credential = %v", err)
	}
	if err := os.Remove(filepath.Join(auth, "linked.json")); err != nil {
		t.Fatal(err)
	}

	// An unreadable auth directory is an error, not an empty snapshot.
	if err := os.RemoveAll(auth); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auth, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectLocalFiles(); err == nil ||
		!strings.Contains(err.Error(), "read auth directory") {
		t.Fatalf("collectLocalFiles() with a file in place of auth/ = %v", err)
	}
}

// TestCollectLocalFilesRefusesAnOversizedConfiguration covers the size guard
// that keeps a push from uploading more than the format allows.
func TestCollectLocalFilesRefusesAnOversizedConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ccl"), 0o700); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(home, ".ccl", "config.yaml")
	if err := os.Truncate(oversized, maxEncryptedSize+1); err != nil {
		if err := os.WriteFile(oversized, make([]byte, maxEncryptedSize+1), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Remove(oversized) })
	if _, _, err := collectLocalFiles(); err == nil ||
		!strings.Contains(err.Error(), "sync safety limit") {
		t.Fatalf("oversized configuration error = %v", err)
	}
}

// TestCollectLocalFilesRejectsAnItselfUnusableConfigPath pins the wrapping: the
// config read failure names the file it could not read.
func TestCollectLocalFilesRejectsAnItselfUnusableConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".ccl", "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collectLocalFiles(); err == nil ||
		!strings.Contains(err.Error(), "read ccl config") {
		t.Fatalf("directory in place of config.yaml error = %v", err)
	}
}

// TestSnapshotPathsAreValidatedWithPathRules documents that the whitelist is
// evaluated on cleaned, slash-separated paths.
func TestSnapshotPathsAreValidatedWithPathRules(t *testing.T) {
	if !strings.EqualFold(path.Ext("auth/CREDENTIAL.JSON"), ".json") {
		t.Fatal("the whitelist is expected to fold the extension case")
	}
}
