package cloudsync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxPairingObjectSize = 64 << 10

type pairingStore interface {
	PutRequest(context.Context, pairingRequestEnvelope) error
	ListRequests(context.Context) ([]pairingRequestEnvelope, error)
	PutResponse(context.Context, pairingResponseEnvelope) error
	GetResponse(context.Context, string) (pairingResponseEnvelope, bool, error)
	Delete(context.Context, string) error
}

type filePairingStore struct {
	root string
}

func newFilePairingStore(remoteDir string) (*filePairingStore, error) {
	if !filepath.IsAbs(remoteDir) {
		return nil, fmt.Errorf("invalid pairing store path")
	}
	return &filePairingStore{root: filepath.Join(remoteDir, pairingDirName)}, nil
}

func (store *filePairingStore) PutRequest(_ context.Context, request pairingRequestEnvelope) error {
	if err := validatePairingRequestEnvelope(request, nowUTC()); err != nil {
		return err
	}
	return store.put("requests", request.RequestID, request)
}

func (store *filePairingStore) ListRequests(_ context.Context) ([]pairingRequestEnvelope, error) {
	directory := filepath.Join(store.root, "requests")
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if len(entries) > 256 {
		return nil, fmt.Errorf("pairing store contains too many requests")
	}
	requests := make([]pairingRequestEnvelope, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".ccl" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".ccl")
		if !validIdentifier(id, 32) {
			return nil, fmt.Errorf("pairing store contains invalid request name %q", entry.Name())
		}
		var request pairingRequestEnvelope
		if err := readLimitedJSONFile(filepath.Join(directory, entry.Name()), &request); err != nil {
			return nil, err
		}
		if request.RequestID != id {
			return nil, fmt.Errorf("pairing request filename does not match its id")
		}
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	return requests, nil
}

func (store *filePairingStore) PutResponse(_ context.Context, response pairingResponseEnvelope) error {
	if !validIdentifier(response.RequestID, 32) {
		return fmt.Errorf("invalid pairing response id")
	}
	return store.put("responses", response.RequestID, response)
}

func (store *filePairingStore) GetResponse(
	_ context.Context,
	requestID string,
) (pairingResponseEnvelope, bool, error) {
	if !validIdentifier(requestID, 32) {
		return pairingResponseEnvelope{}, false, fmt.Errorf("invalid pairing request id")
	}
	path := filepath.Join(store.root, "responses", requestID+".ccl")
	var response pairingResponseEnvelope
	if err := readLimitedJSONFile(path, &response); err != nil {
		if os.IsNotExist(err) {
			return pairingResponseEnvelope{}, false, nil
		}
		return pairingResponseEnvelope{}, false, err
	}
	return response, true, nil
}

func (store *filePairingStore) Delete(_ context.Context, requestID string) error {
	if !validIdentifier(requestID, 32) {
		return fmt.Errorf("invalid pairing request id")
	}
	for _, kind := range []string{"requests", "responses"} {
		path := filepath.Join(store.root, kind, requestID+".ccl")
		if filepath.Dir(path) != filepath.Join(store.root, kind) {
			return fmt.Errorf("refuse to delete invalid pairing path")
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (store *filePairingStore) put(kind, requestID string, value any) error {
	if (kind != "requests" && kind != "responses") || !validIdentifier(requestID, 32) {
		return fmt.Errorf("invalid pairing object")
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxPairingObjectSize {
		return fmt.Errorf("pairing object exceeds safety limit")
	}
	path := filepath.Join(store.root, kind, requestID+".ccl")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("pairing object %s already exists", requestID)
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeAtomic(path, data, 0o600)
}

func readLimitedJSONFile(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to read non-regular pairing object")
	}
	if info.Size() > maxPairingObjectSize {
		return fmt.Errorf("pairing object exceeds safety limit")
	}
	return readJSONFile(path, target)
}

type googlePairingStore struct {
	remote *googleDriveRemote
}

func (store *googlePairingStore) PutRequest(
	ctx context.Context,
	request pairingRequestEnvelope,
) error {
	if err := validatePairingRequestEnvelope(request, nowUTC()); err != nil {
		return err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return store.remote.createApplicationObject(
		ctx, googlePairingObjectName("request", request.RequestID), data,
	)
}

func (store *googlePairingStore) ListRequests(
	ctx context.Context,
) ([]pairingRequestEnvelope, error) {
	objects, err := store.remote.listApplicationObjects(ctx, "ccl-pair-request-")
	if err != nil {
		return nil, err
	}
	if len(objects) > 256 {
		return nil, fmt.Errorf("pairing store contains too many requests")
	}
	requests := make([]pairingRequestEnvelope, 0, len(objects))
	for _, object := range objects {
		data, err := store.remote.downloadApplicationObject(ctx, object)
		if err != nil {
			return nil, err
		}
		var request pairingRequestEnvelope
		if err := json.Unmarshal(data, &request); err != nil {
			return nil, fmt.Errorf("decode Google Drive pairing request: %w", err)
		}
		if object.Name != googlePairingObjectName("request", request.RequestID) {
			return nil, fmt.Errorf("Google Drive pairing request name does not match its id")
		}
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool {
		return requests[i].CreatedAt.Before(requests[j].CreatedAt)
	})
	return requests, nil
}

func (store *googlePairingStore) PutResponse(
	ctx context.Context,
	response pairingResponseEnvelope,
) error {
	if !validIdentifier(response.RequestID, 32) {
		return fmt.Errorf("invalid pairing response id")
	}
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return store.remote.createApplicationObject(
		ctx, googlePairingObjectName("response", response.RequestID), data,
	)
}

func (store *googlePairingStore) GetResponse(
	ctx context.Context,
	requestID string,
) (pairingResponseEnvelope, bool, error) {
	if !validIdentifier(requestID, 32) {
		return pairingResponseEnvelope{}, false, fmt.Errorf("invalid pairing request id")
	}
	object, found, err := store.remote.findApplicationObject(
		ctx, googlePairingObjectName("response", requestID),
	)
	if err != nil || !found {
		return pairingResponseEnvelope{}, found, err
	}
	data, err := store.remote.downloadApplicationObject(ctx, object)
	if err != nil {
		return pairingResponseEnvelope{}, false, err
	}
	var response pairingResponseEnvelope
	if err := json.Unmarshal(data, &response); err != nil {
		return pairingResponseEnvelope{}, false, err
	}
	return response, true, nil
}

func (store *googlePairingStore) Delete(ctx context.Context, requestID string) error {
	if !validIdentifier(requestID, 32) {
		return fmt.Errorf("invalid pairing request id")
	}
	for _, kind := range []string{"request", "response"} {
		name := googlePairingObjectName(kind, requestID)
		object, found, err := store.remote.findApplicationObject(ctx, name)
		if err != nil {
			return err
		}
		if found {
			if err := store.remote.deleteApplicationObject(ctx, object.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func googlePairingObjectName(kind, requestID string) string {
	return "ccl-pair-" + kind + "-" + requestID + ".ccl"
}

func pairingStoreForManager(ctx context.Context, manager *Manager) (pairingStore, error) {
	switch manager.provider {
	case providerICloud:
		return newFilePairingStore(manager.remoteDir)
	case providerGoogleDrive:
		remote, err := loadAuthorizedGoogleDriveAt(
			ctx, remoteAuthPath(manager.localDir, manager.remoteID), manager.alias,
		)
		if err != nil {
			return nil, err
		}
		return &googlePairingStore{remote: remote}, nil
	default:
		return nil, fmt.Errorf("provider %s does not support device pairing", manager.provider)
	}
}
