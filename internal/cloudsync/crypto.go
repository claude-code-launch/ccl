package cloudsync

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/scrypt"
)

var envelopeMagic = []byte("CCLSYNC1")

func newPassphraseProfile() (remoteProfile, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return remoteProfile{}, fmt.Errorf("generate sync salt: %w", err)
	}
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		return remoteProfile{}, fmt.Errorf("generate sync profile id: %w", err)
	}
	return remoteProfile{
		Version: formatVersion,
		ID:      profileID,
		KDF:     kdfScrypt,
		N:       1 << 15,
		R:       8,
		P:       1,
		Salt:    base64.RawStdEncoding.EncodeToString(salt),
	}, nil
}

func newMasterKeyProfile() (remoteProfile, []byte, error) {
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		return remoteProfile{}, nil, fmt.Errorf("generate sync profile id: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return remoteProfile{}, nil, fmt.Errorf("generate sync encryption key: %w", err)
	}
	return remoteProfile{
		Version: formatVersion,
		ID:      profileID,
		KDF:     kdfMasterKey,
	}, key, nil
}

func deriveKey(passphrase string, profile remoteProfile) ([]byte, error) {
	if len(passphrase) < 12 {
		return nil, fmt.Errorf("sync passphrase must contain at least 12 characters")
	}
	if profile.Version != formatVersion || len(profile.ID) != 32 || profile.KDF != kdfScrypt ||
		profile.N != 1<<15 || profile.R != 8 || profile.P != 1 {
		if profile.Version == formatVersion && profile.KDF == kdfMasterKey {
			return nil, fmt.Errorf("this cloud sync profile uses a random recovery key; log in normally or run `ccl cloud key import`")
		}
		return nil, fmt.Errorf("unsupported or unsafe cloud sync profile")
	}
	salt, err := base64.RawStdEncoding.DecodeString(profile.Salt)
	if err != nil || len(salt) != 16 {
		return nil, fmt.Errorf("invalid cloud sync salt")
	}
	key, err := scrypt.Key([]byte(passphrase), salt, profile.N, profile.R, profile.P, 32)
	if err != nil {
		return nil, fmt.Errorf("derive encryption key: %w", err)
	}
	return key, nil
}

func validateRemoteProfile(profile remoteProfile) error {
	if profile.Version != formatVersion || len(profile.ID) != 32 {
		return fmt.Errorf("unsupported or invalid cloud sync profile")
	}
	if _, err := hex.DecodeString(profile.ID); err != nil {
		return fmt.Errorf("invalid cloud sync profile id")
	}
	switch profile.KDF {
	case kdfScrypt:
		if profile.N != 1<<15 || profile.R != 8 || profile.P != 1 {
			return fmt.Errorf("unsupported or unsafe cloud sync profile")
		}
		salt, err := base64.RawStdEncoding.DecodeString(profile.Salt)
		if err != nil || len(salt) != 16 {
			return fmt.Errorf("invalid cloud sync salt")
		}
	case kdfMasterKey:
		if profile.N != 0 || profile.R != 0 || profile.P != 0 || profile.Salt != "" {
			return fmt.Errorf("invalid random-key cloud sync profile")
		}
	default:
		return fmt.Errorf("unsupported cloud sync key type %q", profile.KDF)
	}
	return validatePairingPublicKey(profile.PairingPublicKey)
}

const recoveryKeyPrefix = "CCL1"

func encodeRecoveryKey(profileID string, key []byte) (string, error) {
	id, err := hex.DecodeString(profileID)
	if err != nil || len(id) != 16 || len(key) != 32 {
		return "", fmt.Errorf("invalid profile or encryption key")
	}
	payload := make([]byte, 0, 53)
	payload = append(payload, 1)
	payload = append(payload, id...)
	payload = append(payload, key...)
	sum := sha256.Sum256(append([]byte("ccl-recovery-key:"), payload...))
	payload = append(payload, sum[:4]...)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(payload)
	var groups []string
	for len(encoded) > 0 {
		size := 5
		if len(encoded) < size {
			size = len(encoded)
		}
		groups = append(groups, encoded[:size])
		encoded = encoded[size:]
	}
	return recoveryKeyPrefix + "-" + strings.Join(groups, "-"), nil
}

func decodeRecoveryKey(value string) (string, []byte, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	normalized = strings.NewReplacer("-", "", " ", "", "\n", "", "\r", "", "\t", "").Replace(normalized)
	if !strings.HasPrefix(normalized, recoveryKeyPrefix) {
		return "", nil, fmt.Errorf("invalid recovery key prefix")
	}
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.TrimPrefix(normalized, recoveryKeyPrefix))
	if err != nil || len(raw) != 53 || raw[0] != 1 {
		return "", nil, fmt.Errorf("invalid recovery key")
	}
	payload := raw[:49]
	sum := sha256.Sum256(append([]byte("ccl-recovery-key:"), payload...))
	if !bytes.Equal(raw[49:], sum[:4]) {
		return "", nil, fmt.Errorf("recovery key checksum does not match")
	}
	return hex.EncodeToString(raw[1:17]), append([]byte(nil), raw[17:49]...), nil
}

func sealCompressed(key, plain []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid encryption key")
	}
	var compressed bytes.Buffer
	zw, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(plain); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	ciphertext := aead.Seal(nil, nonce, compressed.Bytes(), envelopeMagic)
	out := make([]byte, 0, len(envelopeMagic)+len(nonce)+len(ciphertext))
	out = append(out, envelopeMagic...)
	out = append(out, nonce...)
	out = append(out, ciphertext...)
	return out, nil
}

func openCompressed(key, encrypted []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	headerSize := len(envelopeMagic) + aead.NonceSize()
	if len(encrypted) < headerSize || !bytes.Equal(encrypted[:len(envelopeMagic)], envelopeMagic) {
		return nil, fmt.Errorf("invalid encrypted sync envelope")
	}
	nonce := encrypted[len(envelopeMagic):headerSize]
	compressed, err := aead.Open(nil, nonce, encrypted[headerSize:], envelopeMagic)
	if err != nil {
		return nil, fmt.Errorf("decrypt sync data (wrong passphrase/recovery key or damaged cloud file): %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open compressed sync data: %w", err)
	}
	defer zr.Close()
	limited := io.LimitReader(zr, maxEncryptedSize+1)
	plain, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("decompress sync data: %w", err)
	}
	if len(plain) > maxEncryptedSize {
		return nil, errors.New("decompressed sync data exceeds safety limit")
	}
	return plain, nil
}
