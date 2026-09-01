package cloudsync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type preparedPush struct {
	files     []snapshotFile
	hash      string
	tag       string
	explicit  bool
	id        string
	createdAt time.Time
	encrypted []byte
}

type pushPlan struct {
	manager       *Manager
	state         localSyncState
	index         remoteIndex
	indexRevision string
	resultID      string
	upload        bool
	indexChanged  bool
}

func (m *Manager) Push(force bool) (PushResult, error) {
	prepared, err := m.preparePush()
	if err != nil {
		return PushResult{}, err
	}
	plan, err := m.planPush(prepared, force)
	if err != nil {
		return PushResult{}, err
	}
	result, err := plan.commit(prepared, true)
	if err != nil {
		return PushResult{}, err
	}
	return result, nil
}

func (m *Manager) preparePush() (preparedPush, error) {
	files, hash, err := collectLocalFiles()
	if err != nil {
		return preparedPush{}, err
	}
	state, err := m.loadState()
	if err != nil {
		return preparedPush{}, err
	}
	tag := state.PendingTag
	if tag == "" {
		tag = defaultTag
	}
	tag, err = normalizeTag(tag)
	if err != nil {
		return preparedPush{}, err
	}
	if state.PendingHash != "" && state.PendingHash != hash {
		if state.ExplicitTag {
			return preparedPush{}, fmt.Errorf("local configuration changed after tag %q was created; run `ccl tag %s` again", tag, tag)
		}
	}
	createdAt := nowUTC()
	payload := snapshotPayload{
		Version: formatVersion, Hash: hash, Tag: tag,
		DeviceID: m.deviceID, CreatedAt: createdAt, Files: files,
	}
	plain, err := json.Marshal(payload)
	if err != nil {
		return preparedPush{}, err
	}
	encrypted, err := sealCompressed(m.key, plain)
	if err != nil {
		return preparedPush{}, err
	}
	id, err := randomSnapshotID()
	if err != nil {
		return preparedPush{}, fmt.Errorf("generate snapshot id: %w", err)
	}
	return preparedPush{
		files: files, hash: hash, tag: tag, explicit: state.ExplicitTag,
		id: id, createdAt: createdAt, encrypted: encrypted,
	}, nil
}

func (m *Manager) planPush(prepared preparedPush, force bool) (*pushPlan, error) {
	state, err := m.loadState()
	if err != nil {
		return nil, err
	}
	revision, err := m.remoteIndexRevision()
	if err != nil {
		return nil, err
	}
	index, err := m.loadRemoteIndex()
	if err != nil {
		return nil, err
	}
	remoteLatestID := index.Tags[defaultTag]
	remoteLatest := index.Snapshots[remoteLatestID]
	plan := &pushPlan{manager: m, state: state, index: index, indexRevision: revision}
	if taggedID := index.Tags[prepared.tag]; taggedID != "" {
		tagged := index.Snapshots[taggedID]
		if tagged.Hash == prepared.hash {
			plan.resultID = taggedID
			return plan, nil
		}
		if prepared.tag != defaultTag && !force {
			return nil, fmt.Errorf("remote tag %q already points to different data; choose a new tag or use --force", prepared.tag)
		}
	}
	if remoteLatestID != "" && !force {
		switch {
		case state.LastRemoteID == "":
			return nil, fmt.Errorf("the remote already contains a snapshot unknown to this device; run `ccl cloud pull` first or use `ccl cloud push --force`")
		case remoteLatestID != state.LastRemoteID && prepared.hash != state.LastLocalHash:
			return nil, fmt.Errorf("local and remote configurations both changed; pull first or push with an explicit tag and --force")
		case remoteLatestID != state.LastRemoteID:
			return nil, fmt.Errorf("the remote has a newer snapshot; run `ccl cloud pull` before pushing")
		}
	}
	if remoteLatest.Hash == prepared.hash {
		plan.resultID = remoteLatestID
		if index.Tags[prepared.tag] != remoteLatestID {
			plan.index.Tags[prepared.tag] = remoteLatestID
			plan.indexChanged = true
		}
		return plan, nil
	}
	plan.resultID = prepared.id
	plan.upload = true
	return plan, nil
}

func (plan *pushPlan) commit(prepared preparedPush, clearPending bool) (PushResult, error) {
	m := plan.manager
	if plan.upload || plan.indexChanged {
		revision, err := m.remoteIndexRevision()
		if err != nil {
			return PushResult{}, err
		}
		if revision != plan.indexRevision {
			return PushResult{}, fmt.Errorf("cloud remote index changed after preflight; refresh before pushing")
		}
	}
	profileChanged, err := m.ensureRemotePairingPublicKey()
	if err != nil {
		return PushResult{}, err
	}
	if plan.upload {
		objectPath := filepath.Join(m.remoteDir, snapshotsDirectory, prepared.id+".ccl")
		if err := os.MkdirAll(filepath.Dir(objectPath), 0o700); err != nil {
			return PushResult{}, err
		}
		if info, statErr := os.Lstat(objectPath); os.IsNotExist(statErr) {
			if err := writeAtomic(objectPath, prepared.encrypted, 0o600); err != nil {
				return PushResult{}, fmt.Errorf("upload encrypted snapshot: %w", err)
			}
		} else if statErr != nil {
			return PushResult{}, statErr
		} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return PushResult{}, fmt.Errorf("refuse to reuse non-regular encrypted snapshot")
		}
		record := snapshotRecord{
			ID: prepared.id, Hash: prepared.hash, Tag: prepared.tag,
			DeviceID: m.deviceID, CreatedAt: prepared.createdAt,
			Size: int64(len(prepared.encrypted)),
		}
		plan.index.Snapshots[prepared.id] = record
		plan.index.Tags[prepared.tag] = prepared.id
		plan.index.Tags[defaultTag] = prepared.id
		plan.indexChanged = true
	}
	if plan.indexChanged {
		if err := m.saveRemoteIndex(plan.index); err != nil {
			return PushResult{}, fmt.Errorf("publish encrypted remote index: %w", err)
		}
	}
	if (plan.upload || plan.indexChanged || profileChanged) && m.flushRemote != nil {
		if err := m.flushRemote(); err != nil {
			return PushResult{}, fmt.Errorf("publish encrypted remote sync bundle: %w", err)
		}
	}
	plan.state.LastRemoteID = plan.resultID
	plan.state.LastLocalHash = prepared.hash
	if record, ok := plan.index.Snapshots[plan.resultID]; ok && record.CreatedAt.After(plan.state.LastRemoteCreatedAt) {
		plan.state.LastRemoteCreatedAt = record.CreatedAt
	}
	if clearPending {
		plan.state.PendingTag = ""
		plan.state.PendingHash = ""
		plan.state.ExplicitTag = false
	}
	plan.state.LastOperation = "push"
	plan.state.LastSyncAt = nowUTC()
	if err := m.saveState(plan.state); err != nil {
		return PushResult{}, err
	}
	return PushResult{
		Tag: prepared.tag, Hash: prepared.hash, ID: plan.resultID,
		Uploaded: plan.upload,
	}, nil
}

func (m *Manager) remoteIndexRevision() (string, error) {
	path := filepath.Join(m.remoteDir, indexFileName)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("refuse to inspect non-regular cloud index")
	}
	if info.Size() > maxEncryptedSize {
		return "", fmt.Errorf("encrypted cloud index exceeds safety limit")
	}
	data, err := readRegularFile(path, maxEncryptedSize)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (m *Manager) clearPendingPush() error {
	if m.profileStateFile == "" {
		state, err := m.loadState()
		if err != nil {
			return err
		}
		state.PendingTag = ""
		state.PendingHash = ""
		state.ExplicitTag = false
		return m.saveState(state)
	}
	var profile localProfileStateV2
	if err := readJSONFile(m.profileStateFile, &profile); err != nil {
		return err
	}
	profile.PendingTag = ""
	profile.PendingHash = ""
	profile.ExplicitTag = false
	profile.LastOperation = "push"
	profile.LastSyncAt = nowUTC()
	return writeJSONAtomic(m.profileStateFile, profile, 0o600)
}

// preflightOutcome tracks one remote's push from load through commit.
// It extends the public RemotePushOutcome with the plan built during
// preflight so the commit phase can pick it up without a parallel slice.
type preflightOutcome struct {
	Alias  string
	Result PushResult
	Err    error
	plan   *pushPlan
}

// PushRemotes pushes one prepared encrypted snapshot to one or more remotes.
// All targets are preflighted before the first write unless bestEffort is set.
func PushRemotes(to string, all, force, bestEffort bool) ([]RemotePushOutcome, error) {
	aliases, err := remoteAliasesForPush(all, to)
	if err != nil {
		return nil, err
	}
	managers, loadErrors, err := loadPushManagers(aliases, bestEffort)
	if err != nil {
		return nil, err
	}
	preparationManager := firstLoadedManager(managers)
	if preparationManager == nil {
		return nil, fmt.Errorf("no cloud remote could be loaded")
	}
	prepared, err := preparationManager.preparePush()
	if err != nil {
		return nil, err
	}
	var (
		operation    pushOperation
		hasOperation bool
	)
	if len(aliases) > 1 {
		operation, hasOperation, err = resumePushOperation(
			preparationManager, aliases, &prepared,
		)
		if err != nil {
			return nil, err
		}
	}
	outcomes := preflightPushPlans(aliases, managers, loadErrors, prepared, force)
	if preflightFailed(outcomes) && !bestEffort {
		return publicOutcomes(outcomes), firstOutcomeError(outcomes)
	}
	if len(aliases) > 1 && !hasOperation {
		operation, err = createPushOperation(preparationManager, aliases, prepared)
		if err != nil {
			return publicOutcomes(outcomes), err
		}
		hasOperation = true
	}
	plans := make([]*pushPlan, len(aliases))
	for index := range aliases {
		plans[index] = outcomes[index].plan
	}
	succeeded := commitPushPlans(plans, prepared, hasOperation, operation, preparationManager, outcomes)
	return finalizePushOutcome(outcomes, succeeded, hasOperation, preparationManager, operation)
}

// loadPushManagers loads a Manager per alias. In best-effort mode a failed
// load is recorded as a nil manager plus its error for later reporting;
// otherwise the first failure aborts the push with a wrapped error.
func loadPushManagers(aliases []string, bestEffort bool) ([]*Manager, []error, error) {
	managers := make([]*Manager, 0, len(aliases))
	loadErrors := make([]error, 0, len(aliases))
	for _, alias := range aliases {
		manager, err := LoadRemote(alias)
		if err != nil {
			if !bestEffort {
				return nil, nil, fmt.Errorf("%s: %w", alias, err)
			}
			managers = append(managers, nil)
			loadErrors = append(loadErrors, err)
			continue
		}
		managers = append(managers, manager)
		loadErrors = append(loadErrors, nil)
	}
	return managers, loadErrors, nil
}

func firstLoadedManager(managers []*Manager) *Manager {
	for _, manager := range managers {
		if manager != nil {
			return manager
		}
	}
	return nil
}

// preflightPushPlans builds a push plan per alias without writing anything.
// Each plan (or load/plan error) is recorded into the outcome at the same
// index; the plan is also stashed in the outcome so the caller can hand it to
// commitPushPlans.
func preflightPushPlans(
	aliases []string,
	managers []*Manager,
	loadErrors []error,
	prepared preparedPush,
	force bool,
) []preflightOutcome {
	outcomes := make([]preflightOutcome, len(aliases))
	for index, alias := range aliases {
		outcomes[index].Alias = alias
		if managers[index] == nil {
			outcomes[index].Err = loadErrors[index]
			continue
		}
		plan, planErr := managers[index].planPush(prepared, force)
		if planErr != nil {
			outcomes[index].Err = planErr
			continue
		}
		outcomes[index].plan = plan
	}
	return outcomes
}

func preflightFailed(outcomes []preflightOutcome) bool {
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			return true
		}
	}
	return false
}

func firstOutcomeError(outcomes []preflightOutcome) error {
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			return fmt.Errorf("%s: %w", outcome.Alias, outcome.Err)
		}
	}
	return nil
}

// commitPushPlans uploads the planned snapshot to every remote that has a
// plan, skipping remotes already completed in a resumed operation. Returns
// the number of successful remotes.
func commitPushPlans(
	plans []*pushPlan,
	prepared preparedPush,
	hasOperation bool,
	operation pushOperation,
	preparationManager *Manager,
	outcomes []preflightOutcome,
) int {
	succeeded := 0
	for index, plan := range plans {
		if plan == nil {
			continue
		}
		if hasOperation && pushOperationCompleted(operation, plan.manager.remoteID) {
			outcomes[index].Result = PushResult{
				Tag: prepared.tag, Hash: prepared.hash,
				ID: prepared.id, Uploaded: false,
			}
			succeeded++
			continue
		}
		result, commitErr := plan.commit(prepared, false)
		outcomes[index].Result = result
		outcomes[index].Err = commitErr
		if commitErr != nil {
			continue
		}
		succeeded++
		if hasOperation {
			if err := markPushOperationComplete(
				preparationManager.localDir, &operation, plan.manager.remoteID,
			); err != nil {
				outcomes[index].Err = err
			}
		}
	}
	return succeeded
}

// finalizePushOutcome converts the internal outcomes into the public
// RemotePushOutcome slice and decides the overall push verdict: nil when all
// remotes succeeded (clearing pending state), PartialPushError when some
// succeeded, or a plain error when nothing did.
func finalizePushOutcome(
	outcomes []preflightOutcome,
	succeeded int,
	hasOperation bool,
	preparationManager *Manager,
	operation pushOperation,
) ([]RemotePushOutcome, error) {
	public := publicOutcomes(outcomes)
	failed := false
	for _, outcome := range outcomes {
		if outcome.Err != nil {
			failed = true
			break
		}
	}
	if !failed {
		if err := preparationManager.clearPendingPush(); err != nil {
			return public, err
		}
		if hasOperation {
			if err := removePushOperation(preparationManager.localDir, operation); err != nil {
				return public, err
			}
		}
		return public, nil
	}
	if succeeded > 0 {
		return public, &PartialPushError{
			Message: "one or more cloud remotes failed after another remote succeeded; retry to finish the pending push",
		}
	}
	return public, fmt.Errorf("one or more cloud remotes failed; successful remotes were kept and the push can be retried")
}

func publicOutcomes(outcomes []preflightOutcome) []RemotePushOutcome {
	public := make([]RemotePushOutcome, len(outcomes))
	for index, outcome := range outcomes {
		public[index] = RemotePushOutcome{Alias: outcome.Alias, Result: outcome.Result, Err: outcome.Err}
	}
	return public
}

func (m *Manager) Pull(tagValue string, force bool) (PullResult, error) {
	tag, err := normalizeTag(tagValue)
	if err != nil {
		return PullResult{}, err
	}
	index, err := m.loadRemoteIndex()
	if err != nil {
		return PullResult{}, err
	}
	id := index.Tags[tag]
	if id == "" {
		return PullResult{}, fmt.Errorf("remote tag %q does not exist", tag)
	}
	record, ok := index.Snapshots[id]
	if !ok {
		return PullResult{}, fmt.Errorf("remote index references missing snapshot %q", id)
	}
	payload, err := m.loadSnapshot(id)
	if err != nil {
		return PullResult{}, err
	}
	if payload.Hash != record.Hash || payload.Hash != hashSnapshotFiles(payload.Files) {
		return PullResult{}, fmt.Errorf("encrypted snapshot integrity metadata does not match its contents")
	}
	state, err := m.loadState()
	if err != nil {
		return PullResult{}, err
	}
	if tag == defaultTag && !force && !record.CreatedAt.IsZero() &&
		!state.LastRemoteCreatedAt.IsZero() && record.CreatedAt.Before(state.LastRemoteCreatedAt) {
		return PullResult{}, fmt.Errorf(
			"remote snapshot %s is older than the newest snapshot this device has synchronized; this can indicate a cloud rollback - run `ccl cloud pull --force` only if that is intended",
			shortIdentifier(id),
		)
	}

	_, localHash, localErr := collectLocalFiles()
	if localErr != nil && !errors.Is(localErr, ErrNoLocalData) {
		return PullResult{}, localErr
	}
	if localHash == payload.Hash {
		state.LastRemoteID = id
		state.LastLocalHash = localHash
		if record.CreatedAt.After(state.LastRemoteCreatedAt) {
			state.LastRemoteCreatedAt = record.CreatedAt
		}
		state.PendingTag = ""
		state.PendingHash = ""
		state.ExplicitTag = false
		state.LastOperation = "pull"
		state.LastSyncAt = nowUTC()
		if err := m.saveState(state); err != nil {
			return PullResult{}, err
		}
		return PullResult{Tag: tag, Hash: payload.Hash, ID: id, Downloaded: false}, nil
	}
	if localHash != "" && !force {
		if state.LastLocalHash == "" || localHash != state.LastLocalHash {
			return PullResult{}, fmt.Errorf("local configuration has unsynchronized changes; run `ccl cloud push`, tag it, or use `ccl cloud pull --force`")
		}
	}
	backupPath, err := m.backupCurrent(tag)
	if err != nil {
		return PullResult{}, err
	}
	if err := m.applySnapshot(payload.Files); err != nil {
		return PullResult{}, err
	}
	state.LastRemoteID = id
	state.LastLocalHash = payload.Hash
	if record.CreatedAt.After(state.LastRemoteCreatedAt) {
		state.LastRemoteCreatedAt = record.CreatedAt
	}
	state.PendingTag = ""
	state.PendingHash = ""
	state.ExplicitTag = false
	state.LastOperation = "pull"
	state.LastSyncAt = nowUTC()
	if err := m.saveState(state); err != nil {
		return PullResult{}, err
	}
	return PullResult{
		Tag: tag, Hash: payload.Hash, ID: id,
		Downloaded: true, BackupPath: backupPath,
	}, nil
}

func (m *Manager) loadSnapshot(id string) (snapshotPayload, error) {
	if len(id) != 64 {
		return snapshotPayload{}, fmt.Errorf("invalid snapshot id")
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return snapshotPayload{}, fmt.Errorf("invalid snapshot id")
		}
	}
	path := filepath.Join(m.remoteDir, snapshotsDirectory, id+".ccl")
	info, err := os.Lstat(path)
	if err != nil {
		return snapshotPayload{}, fmt.Errorf("read encrypted snapshot: %w", err)
	}
	if info.Size() > maxEncryptedSize {
		return snapshotPayload{}, fmt.Errorf("encrypted snapshot exceeds safety limit")
	}
	encrypted, err := readRegularFile(path, maxEncryptedSize)
	if err != nil {
		return snapshotPayload{}, err
	}
	plain, err := openCompressed(m.key, encrypted)
	if err != nil {
		return snapshotPayload{}, err
	}
	var payload snapshotPayload
	if err := json.Unmarshal(plain, &payload); err != nil {
		return snapshotPayload{}, fmt.Errorf("decode encrypted snapshot: %w", err)
	}
	if payload.Version != formatVersion {
		return snapshotPayload{}, fmt.Errorf("unsupported snapshot version %d", payload.Version)
	}
	if err := validateSnapshotFiles(payload.Files); err != nil {
		return snapshotPayload{}, err
	}
	return payload, nil
}
