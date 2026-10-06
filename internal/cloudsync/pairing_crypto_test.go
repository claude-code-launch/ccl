package cloudsync

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// pairingFixture is one fully paired profile: a master key, a device that
// issued a request, and the request itself.
type pairingFixture struct {
	masterKey  []byte
	profile    remoteProfile
	deviceID   string
	deviceName string
	request    pairingRequestEnvelope
	privateKey []byte
}

func newPairingFixture(t *testing.T) pairingFixture {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x11}, 32)
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	public, err := pairingPublicKey(masterKey, profileID)
	if err != nil {
		t.Fatal(err)
	}
	profile := remoteProfile{
		Version: formatVersion, ID: profileID,
		KDF: kdfMasterKey, PairingPublicKey: public,
	}
	deviceID, err := randomDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	request, privateKey, err := newPairingRequest(profile, deviceID, "Test MacBook")
	if err != nil {
		t.Fatalf("newPairingRequest() error = %v", err)
	}
	return pairingFixture{
		masterKey: masterKey, profile: profile,
		deviceID: deviceID, deviceName: "Test MacBook",
		request: request, privateKey: privateKey,
	}
}

// TestPairingHandshakeRoundTrip drives the whole protocol the way two devices
// do: the new device issues a request, the paired device opens and approves it,
// and the new device opens the response.
func TestPairingHandshakeRoundTrip(t *testing.T) {
	fixture := newPairingFixture(t)
	if len(fixture.privateKey) != 32 {
		t.Fatalf("ephemeral private key = %d bytes", len(fixture.privateKey))
	}

	payload, err := openPairingRequest(fixture.masterKey, fixture.request)
	if err != nil {
		t.Fatalf("openPairingRequest() error = %v", err)
	}
	if payload.DeviceID != fixture.deviceID || payload.DeviceName != fixture.deviceName ||
		payload.RequestID != fixture.request.RequestID {
		t.Fatalf("opened payload = %+v", payload)
	}

	response, err := newPairingResponse(fixture.masterKey, fixture.request, payload)
	if err != nil {
		t.Fatalf("newPairingResponse() error = %v", err)
	}
	key, err := openPairingResponse(
		fixture.privateKey, fixture.profile.PairingPublicKey,
		fixture.request, response, fixture.deviceID,
	)
	if err != nil || !bytes.Equal(key, fixture.masterKey) {
		t.Fatalf("openPairingResponse() = %d bytes, %v", len(key), err)
	}

	// The code the user reads is derived from the request, so both devices can
	// display and compare it.
	code := pairingCode(fixture.request)
	if normalizePairingCode(code) != code {
		t.Fatalf("pairingCode() = %q is not a normalized code", code)
	}
	if normalizePairingCode(strings.ToLower(strings.ReplaceAll(code, "-", " "))) != code {
		t.Fatal("a lowercase, space-separated code was not accepted")
	}
}

// TestOpenPairingRequestRejectsTamperingAndExpiry covers everything the
// approving device must refuse: a re-authored envelope, the wrong master key,
// and a request that aged out.
func TestOpenPairingRequestRejectsTamperingAndExpiry(t *testing.T) {
	fixture := newPairingFixture(t)

	otherKey := bytes.Repeat([]byte{0x22}, 32)
	if _, err := openPairingRequest(otherKey, fixture.request); err == nil {
		t.Fatal("another master key opened the request")
	}

	reassigned := fixture.request
	reassigned.ProfileID = strings.Repeat("a", 32)
	if _, err := openPairingRequest(fixture.masterKey, reassigned); err == nil {
		t.Fatal("a request with a different profile id was accepted")
	}

	tampered := fixture.request
	tampered.Ciphertext = flipBase64(t, tampered.Ciphertext)
	if _, err := openPairingRequest(fixture.masterKey, tampered); err == nil ||
		!strings.Contains(err.Error(), "authenticate pairing envelope") {
		t.Fatalf("tampered request error = %v", err)
	}

	expired := fixture.request
	expired.CreatedAt = time.Now().UTC().Add(-2 * pairingLifetime)
	expired.ExpiresAt = expired.CreatedAt.Add(pairingLifetime)
	if _, err := openPairingRequest(fixture.masterKey, expired); err == nil ||
		!strings.Contains(err.Error(), "invalid or expired pairing request") {
		t.Fatalf("expired request error = %v", err)
	}
}

func TestOpenPairingResponseRejectsMismatches(t *testing.T) {
	fixture := newPairingFixture(t)
	payload, err := openPairingRequest(fixture.masterKey, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := newPairingResponse(fixture.masterKey, fixture.request, payload)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := openPairingResponse(
		fixture.privateKey, fixture.profile.PairingPublicKey,
		fixture.request, response, strings.Repeat("9", 32),
	); err == nil || !strings.Contains(err.Error(), "different request or device") {
		t.Fatalf("response for another device error = %v", err)
	}

	// A response whose verifier was minted for another profile is refused, so a
	// replayed response cannot install a key the profile does not own.
	otherProfile := fixture.profile
	otherProfile.ID = strings.Repeat("b", 32)
	otherRequest := fixture.request
	otherRequest.ProfileID = otherProfile.ID
	if _, err := openPairingResponse(
		fixture.privateKey, fixture.profile.PairingPublicKey,
		otherRequest, response, fixture.deviceID,
	); err == nil {
		t.Fatal("a response for another profile was accepted")
	}

	truncatedKey := response
	truncatedKey.Ciphertext = flipBase64(t, truncatedKey.Ciphertext)
	if _, err := openPairingResponse(
		fixture.privateKey, fixture.profile.PairingPublicKey,
		fixture.request, truncatedKey, fixture.deviceID,
	); err == nil {
		t.Fatal("a tampered response was accepted")
	}

	// A private key that did not issue the request cannot read the response.
	_, otherPrivate, err := newPairingRequest(fixture.profile, fixture.deviceID, "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openPairingResponse(
		otherPrivate, fixture.profile.PairingPublicKey,
		fixture.request, response, fixture.deviceID,
	); err == nil {
		t.Fatal("a foreign private key opened the response")
	}

	if _, err := openPairingResponse(
		fixture.privateKey, fixture.profile.PairingPublicKey,
		fixture.request, response, fixture.deviceID,
	); err != nil {
		t.Fatalf("the legitimate response was rejected: %v", err)
	}
}

func TestPairingEnvelopeValidationRejectsMalformedFields(t *testing.T) {
	fixture := newPairingFixture(t)
	now := time.Now().UTC()

	for name, broken := range map[string]pairingRequestEnvelope{
		"wrong version":  {Version: 99, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: now, ExpiresAt: now.Add(time.Minute), EphemeralPublicKey: fixture.request.EphemeralPublicKey},
		"missing nonce":  {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Ciphertext: "c", CreatedAt: now, ExpiresAt: now.Add(time.Minute), EphemeralPublicKey: fixture.request.EphemeralPublicKey},
		"no expiry":      {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: now, EphemeralPublicKey: fixture.request.EphemeralPublicKey},
		"backwards time": {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: now, ExpiresAt: now.Add(-time.Minute), EphemeralPublicKey: fixture.request.EphemeralPublicKey},
		"too long":       {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: now, ExpiresAt: now.Add(2 * pairingLifetime), EphemeralPublicKey: fixture.request.EphemeralPublicKey},
		"from the future": {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c",
			CreatedAt: now.Add(2 * time.Minute), ExpiresAt: now.Add(2*time.Minute + pairingLifetime), EphemeralPublicKey: fixture.request.EphemeralPublicKey},
		"bad public key": {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: now, ExpiresAt: now.Add(time.Minute), EphemeralPublicKey: "!!!"},
		"short public key": {Version: pairingProtocolVersion, RequestID: fixture.request.RequestID, ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
			EphemeralPublicKey: base64RawURL(make([]byte, 16))},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePairingRequestEnvelope(broken, now); err == nil {
				t.Fatalf("validatePairingRequestEnvelope(%+v) accepted a malformed envelope", broken)
			}
		})
	}
	if err := validatePairingRequestEnvelope(fixture.request, now); err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}

	valid := pairingResponseEnvelope{
		Version: pairingProtocolVersion, RequestID: fixture.request.RequestID,
		ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c",
		CreatedAt: fixture.request.CreatedAt, ExpiresAt: fixture.request.ExpiresAt,
	}
	if err := validatePairingResponseEnvelope(valid, fixture.request, now); err != nil {
		t.Fatalf("a valid response was rejected: %v", err)
	}
	for name, broken := range map[string]pairingResponseEnvelope{
		"wrong request":  {Version: pairingProtocolVersion, RequestID: strings.Repeat("c", 32), ProfileID: fixture.request.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: valid.CreatedAt, ExpiresAt: valid.ExpiresAt},
		"wrong profile":  {Version: pairingProtocolVersion, RequestID: valid.RequestID, ProfileID: strings.Repeat("c", 32), Nonce: "n", Ciphertext: "c", CreatedAt: valid.CreatedAt, ExpiresAt: valid.ExpiresAt},
		"early":          {Version: pairingProtocolVersion, RequestID: valid.RequestID, ProfileID: valid.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: valid.CreatedAt.Add(-time.Minute), ExpiresAt: valid.ExpiresAt},
		"after expiry":   {Version: pairingProtocolVersion, RequestID: valid.RequestID, ProfileID: valid.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: valid.ExpiresAt.Add(time.Second), ExpiresAt: valid.ExpiresAt.Add(time.Second)},
		"other lifetime": {Version: pairingProtocolVersion, RequestID: valid.RequestID, ProfileID: valid.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: valid.CreatedAt, ExpiresAt: valid.ExpiresAt.Add(time.Minute)},
		"expired":        {Version: pairingProtocolVersion, RequestID: valid.RequestID, ProfileID: valid.ProfileID, Nonce: "n", Ciphertext: "c", CreatedAt: valid.CreatedAt.Add(-2 * pairingLifetime), ExpiresAt: valid.ExpiresAt.Add(-2 * pairingLifetime)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePairingResponseEnvelope(broken, fixture.request, now); err == nil {
				t.Fatalf("validatePairingResponseEnvelope(%+v) accepted a malformed envelope", broken)
			}
		})
	}
}

func TestNewPairingRequestRefusesUnusableInputs(t *testing.T) {
	masterKey := bytes.Repeat([]byte{0x33}, 32)
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	public, err := pairingPublicKey(masterKey, profileID)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := randomDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	ready := remoteProfile{
		Version: formatVersion, ID: profileID,
		KDF: kdfMasterKey, PairingPublicKey: public,
	}

	notPairing := ready
	notPairing.PairingPublicKey = ""
	if _, _, err := newPairingRequest(notPairing, deviceID, "Mac"); err == nil ||
		!strings.Contains(err.Error(), "not pairing-enabled") {
		t.Fatalf("profile without a pairing key error = %v", err)
	}
	if _, _, err := newPairingRequest(
		remoteProfile{Version: 99, ID: profileID, KDF: kdfScrypt},
		deviceID, "Mac",
	); err == nil {
		t.Fatal("an invalid profile was accepted")
	}
	if _, _, err := newPairingRequest(ready, "short", "Mac"); err == nil ||
		!strings.Contains(err.Error(), "invalid pairing device id") {
		t.Fatalf("short device id error = %v", err)
	}
	for name, deviceName := range map[string]string{
		"empty":    "",
		"blank":    "   ",
		"too long": strings.Repeat("x", 81),
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := newPairingRequest(ready, deviceID, deviceName); err == nil ||
				!strings.Contains(err.Error(), "1 to 80 characters") {
				t.Fatalf("device name %q error = %v", deviceName, err)
			}
		})
	}
	// A device name with surrounding spaces is stored trimmed.
	request, _, err := newPairingRequest(ready, deviceID, "  Mac  ")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := openPairingRequest(masterKey, request)
	if err != nil || payload.DeviceName != "Mac" {
		t.Fatalf("stored device name = %q, %v", payload.DeviceName, err)
	}

	// A pairing key that parses but is a low-order point cannot complete ECDH.
	unusable := ready
	unusable.PairingPublicKey = base64RawURL(make([]byte, 32))
	if _, _, err := newPairingRequest(unusable, deviceID, "Mac"); err == nil {
		t.Fatal("a low-order pairing key was accepted")
	}
}

func TestPairingKeyMaterialRequiresValidIdentifiers(t *testing.T) {
	masterKey := bytes.Repeat([]byte{0x44}, 32)
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pairingPrivateKey(masterKey, profileID); err != nil {
		t.Fatalf("pairingPrivateKey() error = %v", err)
	}
	for name, testCase := range map[string]struct {
		key       []byte
		profileID string
	}{
		"short key":  {key: masterKey[:16], profileID: profileID},
		"short id":   {key: masterKey, profileID: "abc"},
		"non-hex id": {key: masterKey, profileID: strings.Repeat("z", 32)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := pairingPrivateKey(testCase.key, testCase.profileID); err == nil ||
				!strings.Contains(err.Error(), "invalid profile pairing key material") {
				t.Fatalf("pairingPrivateKey() error = %v", err)
			}
		})
	}
	// The derived key is stable, so both devices compute the same static key.
	first, err := pairingPrivateKey(masterKey, profileID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pairingPrivateKey(append([]byte(nil), masterKey...), profileID)
	if err != nil || !first.PublicKey().Equal(second.PublicKey()) {
		t.Fatal("the derived pairing key is not deterministic")
	}
	// Another profile id derives a different key from the same master key.
	otherID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	other, err := pairingPrivateKey(masterKey, otherID)
	if err != nil || first.PublicKey().Equal(other.PublicKey()) {
		t.Fatal("two profiles derived the same pairing key")
	}
}

func TestEnsureProfilePairingPublicKey(t *testing.T) {
	masterKey := bytes.Repeat([]byte{0x55}, 32)
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	profile := remoteProfile{Version: formatVersion, ID: profileID, KDF: kdfMasterKey}

	changed, err := ensureProfilePairingPublicKey(&profile, masterKey)
	if err != nil || !changed || profile.PairingPublicKey == "" {
		t.Fatalf("first call = %v, %v, key %q", changed, err, profile.PairingPublicKey)
	}
	changed, err = ensureProfilePairingPublicKey(&profile, masterKey)
	if err != nil || changed {
		t.Fatalf("second call = %v, %v", changed, err)
	}
	// A key from a different master key is a wrong passphrase, not an upgrade.
	if _, err := ensureProfilePairingPublicKey(&profile, bytes.Repeat([]byte{0x66}, 32)); err == nil ||
		!strings.Contains(err.Error(), "does not match the encryption key") {
		t.Fatalf("mismatched key error = %v", err)
	}
	if _, err := ensureProfilePairingPublicKey(&profile, masterKey[:8]); err == nil {
		t.Fatal("a short master key was accepted")
	}
}

func TestNormalizePairingCodeRejectsUnusableValues(t *testing.T) {
	valid := pairingCode(pairingRequestEnvelope{
		RequestID: strings.Repeat("a", 32), ProfileID: strings.Repeat("b", 32),
		EphemeralPublicKey: base64RawURL(bytes.Repeat([]byte{1}, 32)),
	})
	if normalizePairingCode(valid) != valid {
		t.Fatalf("normalizePairingCode(%q) != itself", valid)
	}
	for _, value := range []string{
		"",
		"ABC",
		"ABCD-EFGH-IJKL-M",
		"ABCD-EFGH-IJK1", // '1' is not in the base32 alphabet
		"ABCD-EFGH-IJK0",
		"!!!!-!!!!-!!!!",
	} {
		if got := normalizePairingCode(value); got != "" {
			t.Fatalf("normalizePairingCode(%q) = %q, want the empty code", value, got)
		}
	}
}

// TestPairingAEADKeyRequiresHexIdentifiers pins the salt derivation: the salt is
// the concatenated ids, so both must be hex.
func TestPairingAEADKeyRequiresHexIdentifiers(t *testing.T) {
	shared := bytes.Repeat([]byte{7}, 32)
	requestID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	key, err := pairingAEADKey(shared, requestID, profileID, "request")
	if err != nil || len(key) != 32 {
		t.Fatalf("pairingAEADKey() = %d bytes, %v", len(key), err)
	}
	// Each of request id, profile id, and purpose is part of the derivation.
	for name, other := range map[string]struct{ requestID, profileID, purpose string }{
		"other request": {requestID: strings.Repeat("c", 32), profileID: profileID, purpose: "request"},
		"other profile": {requestID: requestID, profileID: strings.Repeat("c", 32), purpose: "request"},
		"other purpose": {requestID: requestID, profileID: profileID, purpose: "response"},
	} {
		t.Run(name, func(t *testing.T) {
			otherKey, err := pairingAEADKey(shared, other.requestID, other.profileID, other.purpose)
			if err != nil || bytes.Equal(otherKey, key) {
				t.Fatalf("derived key is not bound to its inputs: %v", err)
			}
		})
	}
	if _, err := pairingAEADKey(shared, "zz", profileID, "request"); err == nil {
		t.Fatal("a non-hex request id was accepted")
	}
}

func TestOpenPairingPayloadRejectsMalformedEncodings(t *testing.T) {
	shared := bytes.Repeat([]byte{8}, 32)
	requestID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	var target map[string]string
	for name, testCase := range map[string]struct{ nonce, ciphertext string }{
		"bad nonce":      {nonce: "!!!", ciphertext: "AAAA"},
		"short nonce":    {nonce: base64RawURL(make([]byte, 8)), ciphertext: "AAAA"},
		"bad ciphertext": {nonce: base64RawURL(make([]byte, 24)), ciphertext: "!!!"},
		"short body":     {nonce: base64RawURL(make([]byte, 24)), ciphertext: base64RawURL(make([]byte, 4))},
	} {
		t.Run(name, func(t *testing.T) {
			err := openPairingPayload(
				shared, requestID, profileID, "request",
				testCase.nonce, testCase.ciphertext, []byte("aad"), &target,
			)
			if err == nil {
				t.Fatal("a malformed payload was opened")
			}
		})
	}

	// A payload sealed for another purpose or another request id does not open.
	nonce, ciphertext, err := sealPairingPayload(
		shared, requestID, profileID, "request", map[string]string{"a": "b"}, []byte("aad"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := openPairingPayload(
		shared, requestID, profileID, "response", nonce, ciphertext, []byte("aad"), &target,
	); err == nil {
		t.Fatal("a payload sealed for another purpose was opened")
	}
	otherRequest, err := randomHexIdentifier(16)
	if err != nil {
		t.Fatal(err)
	}
	if err := openPairingPayload(
		shared, otherRequest, profileID, "request", nonce, ciphertext, []byte("aad"), &target,
	); err == nil {
		t.Fatal("a payload sealed for another request was opened")
	}
	if err := openPairingPayload(
		shared, requestID, profileID, "request", nonce, ciphertext, []byte("other-aad"), &target,
	); err == nil {
		t.Fatal("a payload sealed with another AAD was opened")
	}
	if err := openPairingPayload(
		shared, requestID, profileID, "request", nonce, ciphertext, []byte("aad"), &target,
	); err != nil || target["a"] != "b" {
		t.Fatalf("the legitimate payload did not open: %+v, %v", target, err)
	}
}

// flipBase64 changes one character of a raw-base64url value without changing
// its length, so a tampered envelope still parses.
func flipBase64(t *testing.T, value string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) == 0 {
		t.Fatalf("cannot tamper with %q: %v", value, err)
	}
	raw[0] ^= 0x01
	return base64.RawURLEncoding.EncodeToString(raw)
}

func base64RawURL(raw []byte) string {
	return base64.RawURLEncoding.EncodeToString(raw)
}

// TestPairingCodeDependsOnTheRequest pins that the display code is derived, not
// random: two devices showing different codes must notice.
func TestPairingCodeDependsOnTheRequest(t *testing.T) {
	first := newPairingFixture(t)
	second := newPairingFixture(t)
	if pairingCode(first.request) == pairingCode(second.request) {
		t.Fatal("two distinct requests produced the same pairing code")
	}
	if len(pairingCode(first.request)) != 14 {
		t.Fatalf("pairing code = %q", pairingCode(first.request))
	}
	if strings.ContainsAny(pairingCode(first.request), "0189") {
		// Base32 excludes 0/1/8/9, which is why the code is easy to read aloud.
		t.Fatalf("pairing code contains ambiguous characters: %q", pairingCode(first.request))
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).
		DecodeString(strings.ReplaceAll(pairingCode(first.request), "-", ""))
	if err != nil || len(decoded) < 4 {
		t.Fatalf("pairing code is not base32: %v", err)
	}
}
