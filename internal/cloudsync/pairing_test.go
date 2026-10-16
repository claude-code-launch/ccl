package cloudsync

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevicePairingRoundTripThroughICloud(t *testing.T) {
	oldHome := t.TempDir()
	newHome := t.TempDir()
	drive := t.TempDir()

	t.Setenv("HOME", oldHome)
	t.Setenv("CCL_ICLOUD_DRIVE_DIR", drive)
	writeSyncFixture(t, oldHome)
	oldLogin, err := LoginICloudNamed("personal", false, "")
	if err != nil {
		t.Fatal(err)
	}
	oldManager, err := LoadRemote("personal")
	if err != nil {
		t.Fatal(err)
	}
	oldKey := append([]byte(nil), oldManager.key...)
	if _, err := oldManager.Push(false); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", newHome)
	writeSyncFixture(t, newHome)
	if _, err := LoginICloudNamed("personal", false, ""); err == nil ||
		!strings.Contains(err.Error(), "ccl cloud device request") {
		t.Fatalf("new device login should require pairing: %v", err)
	}
	request, err := StartPairing(t.Context(), "personal", "New MacBook")
	if err != nil {
		t.Fatal(err)
	}
	if normalizePairingCode(request.Code) != request.Code ||
		len(strings.ReplaceAll(request.Code, "-", "")) != 12 {
		t.Fatalf("pairing code = %q", request.Code)
	}

	t.Setenv("HOME", oldHome)
	requests, err := ListPairingRequests(t.Context(), "personal", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Code != request.Code ||
		requests[0].DeviceName != "New MacBook" {
		t.Fatalf("pairing requests = %+v", requests)
	}
	approved, err := ApprovePairing(t.Context(), request.Code, "personal")
	if err != nil {
		t.Fatal(err)
	}
	if approved.DeviceName != "New MacBook" {
		t.Fatalf("approval = %+v", approved)
	}

	t.Setenv("HOME", newHome)
	completed, err := CompletePairing(t.Context(), request.Code)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Alias != "personal" || completed.KeyMode != keyModePairing {
		t.Fatalf("completed pairing = %+v", completed)
	}
	newManager, err := LoadRemote("personal")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(newManager.key, oldKey) {
		t.Fatal("paired device received a different profile key")
	}
	if newManager.deviceID == oldLogin.DeviceID {
		t.Fatal("paired device reused the old device id")
	}
	if _, err := os.Stat(filepath.Join(
		drive, remoteDirectory, pairingDirName, "requests", request.RequestID+".ccl",
	)); !os.IsNotExist(err) {
		t.Fatalf("pairing request was not consumed: %v", err)
	}
}

func TestPairingEnvelopeRejectsTampering(t *testing.T) {
	profile, key, err := newMasterKeyProfile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ensureProfilePairingPublicKey(&profile, key); err != nil {
		t.Fatal(err)
	}
	deviceID, err := randomDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	request, _, err := newPairingRequest(profile, deviceID, "Test device")
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(request.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext[len(ciphertext)-1] ^= 0x01
	request.Ciphertext = base64.RawURLEncoding.EncodeToString(ciphertext)
	if _, err := openPairingRequest(key, request); err == nil {
		t.Fatal("tampered pairing request was accepted")
	}
}
