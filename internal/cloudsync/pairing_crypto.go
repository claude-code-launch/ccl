package cloudsync

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	pairingProtocolVersion = 1
	pairingLifetime        = 10 * time.Minute
)

func pairingPrivateKey(masterKey []byte, profileID string) (*ecdh.PrivateKey, error) {
	if len(masterKey) != 32 || !validIdentifier(profileID, 32) {
		return nil, fmt.Errorf("invalid profile pairing key material")
	}
	salt, err := hex.DecodeString(profileID)
	if err != nil {
		return nil, err
	}
	scalar, err := hkdf.Key(
		sha256.New, masterKey, salt,
		"ccl/profile-pairing-static/x25519/v1", 32,
	)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(scalar)
}

func pairingPublicKey(masterKey []byte, profileID string) (string, error) {
	private, err := pairingPrivateKey(masterKey, profileID)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()), nil
}

func validatePairingPublicKey(value string) error {
	if value == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("invalid cloud profile pairing public key")
	}
	if _, err := ecdh.X25519().NewPublicKey(raw); err != nil {
		return fmt.Errorf("invalid cloud profile pairing public key: %w", err)
	}
	return nil
}

func ensureProfilePairingPublicKey(profile *remoteProfile, masterKey []byte) (bool, error) {
	expected, err := pairingPublicKey(masterKey, profile.ID)
	if err != nil {
		return false, err
	}
	if profile.PairingPublicKey == expected {
		return false, nil
	}
	if profile.PairingPublicKey != "" {
		return false, fmt.Errorf("cloud profile pairing public key does not match the encryption key (wrong passphrase/recovery key)")
	}
	profile.PairingPublicKey = expected
	return true, nil
}

func newPairingRequest(
	profile remoteProfile,
	deviceID, deviceName string,
) (pairingRequestEnvelope, []byte, error) {
	if err := validateRemoteProfile(profile); err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	if profile.PairingPublicKey == "" {
		return pairingRequestEnvelope{}, nil, fmt.Errorf("cloud profile is not pairing-enabled; run `ccl cloud push` on an authorized device first")
	}
	if err := validatePairingPublicKey(profile.PairingPublicKey); err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	if !validIdentifier(deviceID, 32) {
		return pairingRequestEnvelope{}, nil, fmt.Errorf("invalid pairing device id")
	}
	if strings.TrimSpace(deviceName) == "" || len(deviceName) > 80 {
		return pairingRequestEnvelope{}, nil, fmt.Errorf("device name must contain 1 to 80 characters")
	}
	requestID, err := randomHexIdentifier(16)
	if err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	staticRaw, err := base64.RawURLEncoding.DecodeString(profile.PairingPublicKey)
	if err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	staticPublic, err := ecdh.X25519().NewPublicKey(staticRaw)
	if err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	shared, err := private.ECDH(staticPublic)
	if err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	createdAt := nowUTC()
	expiresAt := createdAt.Add(pairingLifetime)
	envelope := pairingRequestEnvelope{
		Version: pairingProtocolVersion, RequestID: requestID,
		ProfileID:          profile.ID,
		EphemeralPublicKey: base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
		CreatedAt:          createdAt, ExpiresAt: expiresAt,
	}
	payload := pairingRequestPayload{
		RequestID: requestID, DeviceID: deviceID,
		DeviceName: strings.TrimSpace(deviceName),
	}
	nonce, ciphertext, err := sealPairingPayload(
		shared, requestID, profile.ID, "request", payload,
		pairingRequestAAD(envelope),
	)
	if err != nil {
		return pairingRequestEnvelope{}, nil, err
	}
	envelope.Nonce = nonce
	envelope.Ciphertext = ciphertext
	return envelope, append([]byte(nil), private.Bytes()...), nil
}

func openPairingRequest(
	masterKey []byte,
	envelope pairingRequestEnvelope,
) (pairingRequestPayload, error) {
	if err := validatePairingRequestEnvelope(envelope, nowUTC()); err != nil {
		return pairingRequestPayload{}, err
	}
	private, err := pairingPrivateKey(masterKey, envelope.ProfileID)
	if err != nil {
		return pairingRequestPayload{}, err
	}
	publicRaw, err := base64.RawURLEncoding.DecodeString(envelope.EphemeralPublicKey)
	if err != nil {
		return pairingRequestPayload{}, err
	}
	public, err := ecdh.X25519().NewPublicKey(publicRaw)
	if err != nil {
		return pairingRequestPayload{}, err
	}
	shared, err := private.ECDH(public)
	if err != nil {
		return pairingRequestPayload{}, err
	}
	var payload pairingRequestPayload
	if err := openPairingPayload(
		shared, envelope.RequestID, envelope.ProfileID, "request",
		envelope.Nonce, envelope.Ciphertext,
		pairingRequestAAD(envelope), &payload,
	); err != nil {
		return pairingRequestPayload{}, err
	}
	if payload.RequestID != envelope.RequestID || !validIdentifier(payload.DeviceID, 32) ||
		strings.TrimSpace(payload.DeviceName) == "" || len(payload.DeviceName) > 80 {
		return pairingRequestPayload{}, fmt.Errorf("invalid encrypted pairing request")
	}
	return payload, nil
}

func newPairingResponse(
	masterKey []byte,
	envelope pairingRequestEnvelope,
	payload pairingRequestPayload,
) (pairingResponseEnvelope, error) {
	if err := validatePairingRequestEnvelope(envelope, nowUTC()); err != nil {
		return pairingResponseEnvelope{}, err
	}
	private, err := pairingPrivateKey(masterKey, envelope.ProfileID)
	if err != nil {
		return pairingResponseEnvelope{}, err
	}
	publicRaw, err := base64.RawURLEncoding.DecodeString(envelope.EphemeralPublicKey)
	if err != nil {
		return pairingResponseEnvelope{}, err
	}
	public, err := ecdh.X25519().NewPublicKey(publicRaw)
	if err != nil {
		return pairingResponseEnvelope{}, err
	}
	shared, err := private.ECDH(public)
	if err != nil {
		return pairingResponseEnvelope{}, err
	}
	createdAt := nowUTC()
	response := pairingResponseEnvelope{
		Version: pairingProtocolVersion, RequestID: envelope.RequestID,
		ProfileID: envelope.ProfileID, CreatedAt: createdAt,
		ExpiresAt: envelope.ExpiresAt,
	}
	verifier := sha256.Sum256([]byte("ccl-sync-profile:" + envelope.ProfileID))
	responsePayload := pairingResponsePayload{
		RequestID: envelope.RequestID, ProfileID: envelope.ProfileID,
		DeviceID:     payload.DeviceID,
		MasterKey:    base64.RawStdEncoding.EncodeToString(masterKey),
		VerifierHash: hex.EncodeToString(verifier[:]),
	}
	nonce, ciphertext, err := sealPairingPayload(
		shared, envelope.RequestID, envelope.ProfileID, "response",
		responsePayload, pairingResponseAAD(response),
	)
	if err != nil {
		return pairingResponseEnvelope{}, err
	}
	response.Nonce = nonce
	response.Ciphertext = ciphertext
	return response, nil
}

func openPairingResponse(
	privateBytes []byte,
	profilePublicKey string,
	request pairingRequestEnvelope,
	response pairingResponseEnvelope,
	deviceID string,
) ([]byte, error) {
	if err := validatePairingRequestEnvelope(request, nowUTC()); err != nil {
		return nil, err
	}
	if err := validatePairingResponseEnvelope(response, request, nowUTC()); err != nil {
		return nil, err
	}
	private, err := ecdh.X25519().NewPrivateKey(privateBytes)
	if err != nil {
		return nil, err
	}
	staticRaw, err := base64.RawURLEncoding.DecodeString(profilePublicKey)
	if err != nil {
		return nil, err
	}
	staticPublic, err := ecdh.X25519().NewPublicKey(staticRaw)
	if err != nil {
		return nil, err
	}
	shared, err := private.ECDH(staticPublic)
	if err != nil {
		return nil, err
	}
	var payload pairingResponsePayload
	if err := openPairingPayload(
		shared, request.RequestID, request.ProfileID, "response",
		response.Nonce, response.Ciphertext,
		pairingResponseAAD(response), &payload,
	); err != nil {
		return nil, err
	}
	if payload.RequestID != request.RequestID || payload.ProfileID != request.ProfileID ||
		payload.DeviceID != deviceID {
		return nil, fmt.Errorf("pairing response is for a different request or device")
	}
	key, err := base64.RawStdEncoding.DecodeString(payload.MasterKey)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("pairing response contains an invalid master key")
	}
	verifier := sha256.Sum256([]byte("ccl-sync-profile:" + request.ProfileID))
	if payload.VerifierHash != hex.EncodeToString(verifier[:]) {
		return nil, fmt.Errorf("pairing response verifier does not match the profile")
	}
	return key, nil
}

func sealPairingPayload(
	shared []byte,
	requestID, profileID, purpose string,
	payload any,
	aad []byte,
) (string, string, error) {
	key, err := pairingAEADKey(shared, requestID, profileID, purpose)
	if err != nil {
		return "", "", err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return "", "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", "", err
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	ciphertext := aead.Seal(nil, nonce, plain, aad)
	return base64.RawURLEncoding.EncodeToString(nonce),
		base64.RawURLEncoding.EncodeToString(ciphertext), nil
}

func openPairingPayload(
	shared []byte,
	requestID, profileID, purpose, nonceValue, ciphertextValue string,
	aad []byte,
	target any,
) error {
	key, err := pairingAEADKey(shared, requestID, profileID, purpose)
	if err != nil {
		return err
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	nonce, err := base64.RawURLEncoding.DecodeString(nonceValue)
	if err != nil || len(nonce) != aead.NonceSize() {
		return fmt.Errorf("invalid pairing nonce")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(ciphertextValue)
	if err != nil || len(ciphertext) < aead.Overhead() {
		return fmt.Errorf("invalid pairing ciphertext")
	}
	plain, err := aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return fmt.Errorf("authenticate pairing envelope: %w", err)
	}
	if err := json.Unmarshal(plain, target); err != nil {
		return fmt.Errorf("decode pairing envelope: %w", err)
	}
	return nil
}

func pairingAEADKey(shared []byte, requestID, profileID, purpose string) ([]byte, error) {
	salt, err := hex.DecodeString(requestID + profileID)
	if err != nil {
		return nil, err
	}
	return hkdf.Key(
		sha256.New, shared, salt,
		"ccl/device-pairing/"+purpose+"/xchacha20poly1305/v1", 32,
	)
}

func pairingRequestAAD(value pairingRequestEnvelope) []byte {
	return []byte(fmt.Sprintf(
		"CCLPAIR1\x00request\x00%s\x00%s\x00%s\x00%s\x00%s",
		value.RequestID, value.ProfileID, value.EphemeralPublicKey,
		value.CreatedAt.UTC().Format(time.RFC3339),
		value.ExpiresAt.UTC().Format(time.RFC3339),
	))
}

func pairingResponseAAD(value pairingResponseEnvelope) []byte {
	return []byte(fmt.Sprintf(
		"CCLPAIR1\x00response\x00%s\x00%s\x00%s\x00%s",
		value.RequestID, value.ProfileID,
		value.CreatedAt.UTC().Format(time.RFC3339),
		value.ExpiresAt.UTC().Format(time.RFC3339),
	))
}

func validatePairingRequestEnvelope(value pairingRequestEnvelope, now time.Time) error {
	if value.Version != pairingProtocolVersion ||
		!validIdentifier(value.RequestID, 32) ||
		!validIdentifier(value.ProfileID, 32) ||
		value.Nonce == "" || value.Ciphertext == "" ||
		value.CreatedAt.IsZero() || value.ExpiresAt.IsZero() ||
		!value.ExpiresAt.After(value.CreatedAt) ||
		value.ExpiresAt.Sub(value.CreatedAt) > pairingLifetime ||
		now.After(value.ExpiresAt) ||
		value.CreatedAt.After(now.Add(time.Minute)) {
		return fmt.Errorf("invalid or expired pairing request")
	}
	public, err := base64.RawURLEncoding.DecodeString(value.EphemeralPublicKey)
	if err != nil || len(public) != 32 {
		return fmt.Errorf("invalid pairing request public key")
	}
	return nil
}

func validatePairingResponseEnvelope(
	value pairingResponseEnvelope,
	request pairingRequestEnvelope,
	now time.Time,
) error {
	if value.Version != pairingProtocolVersion ||
		value.RequestID != request.RequestID ||
		value.ProfileID != request.ProfileID ||
		value.Nonce == "" || value.Ciphertext == "" ||
		value.CreatedAt.Before(request.CreatedAt) ||
		value.CreatedAt.After(value.ExpiresAt) ||
		!value.ExpiresAt.Equal(request.ExpiresAt) ||
		now.After(value.ExpiresAt) {
		return fmt.Errorf("invalid or expired pairing response")
	}
	return nil
}

func pairingCode(value pairingRequestEnvelope) string {
	sum := sha256.Sum256([]byte(
		"ccl-pairing-code\x00" + value.RequestID + "\x00" +
			value.ProfileID + "\x00" + value.EphemeralPublicKey,
	))
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	code = code[:12]
	return code[:4] + "-" + code[4:8] + "-" + code[8:12]
}

func normalizePairingCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, " ", "")
	value = strings.ReplaceAll(value, "-", "")
	if len(value) != 12 {
		return ""
	}
	for _, char := range value {
		if !strings.ContainsRune("ABCDEFGHIJKLMNOPQRSTUVWXYZ234567", char) {
			return ""
		}
	}
	return value[:4] + "-" + value[4:8] + "-" + value[8:]
}
