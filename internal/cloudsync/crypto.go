package cloudsync

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

var envelopeMagic = []byte("CCLSYNC1")

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

func validateRemoteProfile(profile remoteProfile) error {
	if profile.Version != formatVersion || len(profile.ID) != 32 {
		return fmt.Errorf("unsupported or invalid cloud sync profile")
	}
	if _, err := hex.DecodeString(profile.ID); err != nil {
		return fmt.Errorf("invalid cloud sync profile id")
	}
	if profile.KDF != kdfMasterKey {
		return fmt.Errorf("unsupported cloud sync key type %q", profile.KDF)
	}
	if profile.AccountKey != "" {
		if key, err := base64.RawStdEncoding.DecodeString(profile.AccountKey); err != nil || len(key) != 32 {
			return fmt.Errorf("invalid cloud sync account key")
		}
	}
	return nil
}

// ensureProfileAccountKey records key in profile, so signing in to the cloud
// account is enough to open the profile on another device.
func ensureProfileAccountKey(profile *remoteProfile, key []byte) (bool, error) {
	encoded := base64.RawStdEncoding.EncodeToString(key)
	switch profile.AccountKey {
	case encoded:
		return false, nil
	case "":
		profile.AccountKey = encoded
		return true, nil
	}
	return false, fmt.Errorf("cloud sync profile stores a different key")
}

func profileAccountKey(profile remoteProfile) ([]byte, bool) {
	key, err := base64.RawStdEncoding.DecodeString(profile.AccountKey)
	return key, err == nil && len(key) == 32
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
		return nil, fmt.Errorf("decrypt sync data (wrong key or damaged cloud file): %w", err)
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
