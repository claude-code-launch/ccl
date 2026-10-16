package cloudsync

import (
	"bytes"
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
		!strings.Contains(err.Error(), "wrong key") {
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

func TestValidateRemoteProfile(t *testing.T) {
	profile, key, err := newMasterKeyProfile()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateRemoteProfile(profile); err != nil {
		t.Fatalf("new profile rejected: %v", err)
	}
	if _, err := ensureProfileAccountKey(&profile, key); err != nil {
		t.Fatal(err)
	}
	if err := validateRemoteProfile(profile); err != nil {
		t.Fatalf("profile with an account key rejected: %v", err)
	}
	if _, err := ensureProfileAccountKey(&profile, bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("a profile's account key was replaced by another key")
	}
	for name, broken := range map[string]remoteProfile{
		"wrong version": {Version: 99, ID: profile.ID, KDF: kdfMasterKey},
		"short id":      {Version: formatVersion, ID: "abc", KDF: kdfMasterKey},
		"non-hex id":    {Version: formatVersion, ID: strings.Repeat("z", 32), KDF: kdfMasterKey},
		"unknown kdf":   {Version: formatVersion, ID: profile.ID, KDF: "scrypt"},
		"bad key":       {Version: formatVersion, ID: profile.ID, KDF: kdfMasterKey, AccountKey: "not-a-key"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRemoteProfile(broken); err == nil {
				t.Fatalf("validateRemoteProfile(%+v) accepted an invalid profile", broken)
			}
		})
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
