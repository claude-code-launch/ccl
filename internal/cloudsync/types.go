package cloudsync

import "time"

const (
	formatVersion       = 1
	registryVersion     = 2
	providerICloud      = "icloud"
	providerGoogleDrive = "google-drive"
	defaultTag          = "latest"
	remoteDirectory     = "ccl-sync"
	profileFileName     = "profile.json"
	verifierFileName    = "verifier.ccl"
	indexFileName       = "index.ccl"
	snapshotsDirectory  = "snapshots"
	cloudConfigName     = "cloud.json"
	cloudKeyName        = "cloud.key"
	cloudStateName      = "cloud-state.json"
	googleAuthName      = "google-drive-auth.json"
	googleCacheName     = "google-drive-cache"
	googleBundleName    = "ccl-sync.bundle"
	backupsDirectory    = "backups"
	cloudDirectoryName  = "cloud"
	registryFileName    = "registry.json"
	profilesDirName     = "profiles"
	remotesDirName      = "remotes"
	operationsDirName   = "operations"
	profileStateName    = "state.json"
	remoteConfigName    = "remote.json"
	remoteStateName     = "state.json"
	remoteAuthName      = "auth.json"
	remoteCacheName     = "cache"
	maxEncryptedSize    = 64 << 20
	maxBundleSize       = 240 << 20
	keyModeKeychain     = "keychain"
	keyModeLocal        = "local"
	keyModePassphrase   = "passphrase"
	keyModeRecovery     = "recovery"
	keyModePairing      = "pairing" // devices paired before pairing was removed
	kdfScrypt           = "scrypt"
	kdfMasterKey        = "master-key"
)

type remoteProfile struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	KDF     string `json:"kdf"`
	N       int    `json:"n"`
	R       int    `json:"r"`
	P       int    `json:"p"`
	Salt    string `json:"salt"`
	// AccountKey is the profile key itself: access to the cloud account
	// (Google Drive appDataFolder or iCloud Drive) is what protects the
	// data, so a new device needs nothing but a login.
	AccountKey string `json:"account_key,omitempty"`
}

type localCloudConfig struct {
	Version     int    `json:"version"`
	Provider    string `json:"provider"`
	RemoteDir   string `json:"remote_dir"`
	RemoteLabel string `json:"remote_label,omitempty"`
	DeviceID    string `json:"device_id"`
	ProfileID   string `json:"profile_id"`
	KeyMode     string `json:"key_mode,omitempty"`
}

type localSyncState struct {
	LastRemoteID        string    `json:"last_remote_id,omitempty"`
	LastLocalHash       string    `json:"last_local_hash,omitempty"`
	LastRemoteCreatedAt time.Time `json:"last_remote_created_at,omitempty"`
	PendingTag          string    `json:"pending_tag,omitempty"`
	PendingHash         string    `json:"pending_hash,omitempty"`
	ExplicitTag         bool      `json:"explicit_tag,omitempty"`
	LastOperation       string    `json:"last_operation,omitempty"`
	LastSyncAt          time.Time `json:"last_sync_at,omitempty"`
}

type localDevice struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

type cloudRegistry struct {
	Version          int               `json:"version"`
	ActiveProfileID  string            `json:"active_profile_id"`
	PrimaryRemoteID  string            `json:"primary_remote_id,omitempty"`
	Device           localDevice       `json:"device"`
	Aliases          map[string]string `json:"aliases"`
	RemoteOrder      []string          `json:"remote_order"`
	MigratedFromV1At time.Time         `json:"migrated_from_v1_at,omitempty"`
}

type localProfileStateV2 struct {
	Version       int       `json:"version"`
	ProfileID     string    `json:"profile_id"`
	KeyMode       string    `json:"key_mode"`
	PendingTag    string    `json:"pending_tag,omitempty"`
	PendingHash   string    `json:"pending_hash,omitempty"`
	ExplicitTag   bool      `json:"explicit_tag,omitempty"`
	LastOperation string    `json:"last_operation,omitempty"`
	LastSyncAt    time.Time `json:"last_sync_at,omitempty"`
}

type localRemoteConfigV2 struct {
	Version        int               `json:"version"`
	ID             string            `json:"id"`
	Alias          string            `json:"alias"`
	Provider       string            `json:"provider"`
	ProfileID      string            `json:"profile_id"`
	RemoteDir      string            `json:"remote_dir"`
	RemoteLabel    string            `json:"remote_label,omitempty"`
	Enabled        bool              `json:"enabled"`
	Mirror         bool              `json:"mirror"`
	AccountID      string            `json:"account_id,omitempty"`
	AccountHint    string            `json:"account_hint,omitempty"`
	ProviderConfig map[string]string `json:"provider_config,omitempty"`
}

type localRemoteStateV2 struct {
	Version              int       `json:"version"`
	LastSeenRemoteID     string    `json:"last_seen_remote_id,omitempty"`
	LastPushedSnapshotID string    `json:"last_pushed_snapshot_id,omitempty"`
	LastPulledSnapshotID string    `json:"last_pulled_snapshot_id,omitempty"`
	LastRemoteID         string    `json:"last_remote_id,omitempty"`
	LastLocalHash        string    `json:"last_local_hash,omitempty"`
	LastRemoteHash       string    `json:"last_remote_hash,omitempty"`
	LastRemoteCreatedAt  time.Time `json:"last_remote_created_at,omitempty"`
	LastOperation        string    `json:"last_operation,omitempty"`
	LastSyncAt           time.Time `json:"last_sync_at,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
}

type remoteIndex struct {
	Version   int                       `json:"version"`
	Tags      map[string]string         `json:"tags"`
	Snapshots map[string]snapshotRecord `json:"snapshots"`
}

type snapshotRecord struct {
	ID        string    `json:"id"`
	Hash      string    `json:"hash"`
	Tag       string    `json:"tag"`
	DeviceID  string    `json:"device_id"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size"`
}

type snapshotPayload struct {
	Version   int            `json:"version"`
	Hash      string         `json:"hash"`
	Tag       string         `json:"tag"`
	DeviceID  string         `json:"device_id"`
	CreatedAt time.Time      `json:"created_at"`
	Files     []snapshotFile `json:"files"`
}

type snapshotFile struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
	Data []byte `json:"data"`
}

type LoginResult struct {
	RemoteDir string
	DeviceID  string
	Alias     string
	Provider  string
	Existing  bool
	KeyMode   string
	Migrated  bool
}

type KeyExportResult struct {
	ProfileID   string
	RecoveryKey string
	KeyMode     string
}

type KeyImportResult struct {
	RemoteDir string
	DeviceID  string
	KeyMode   string
}

type TagResult struct {
	Tag  string
	Hash string
}

type PushResult struct {
	Tag      string
	Hash     string
	ID       string
	Uploaded bool
}

type RemotePushOutcome struct {
	Alias  string
	Result PushResult
	Err    error
}

type PartialPushError struct {
	Message string
}

func (err *PartialPushError) Error() string {
	if err == nil || err.Message == "" {
		return "cloud push partially completed"
	}
	return err.Message
}

func (*PartialPushError) ExitCode() int {
	return 2
}

type pushOperation struct {
	Version      int       `json:"version"`
	ID           string    `json:"id"`
	ProfileID    string    `json:"profile_id"`
	SnapshotID   string    `json:"snapshot_id"`
	Hash         string    `json:"hash"`
	Tag          string    `json:"tag"`
	TargetIDs    []string  `json:"target_ids"`
	CompletedIDs []string  `json:"completed_ids,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type PullResult struct {
	Tag        string
	Hash       string
	ID         string
	Downloaded bool
	BackupPath string
}

type Status struct {
	Alias          string
	Primary        bool
	Mirror         bool
	Provider       string
	KeyMode        string
	RemoteDir      string
	DeviceID       string
	LocalHash      string
	PendingTag     string
	RemoteTag      string
	RemoteID       string
	RemoteHash     string
	RemoteDeviceID string
	RemoteCreated  time.Time
	LastOperation  string
	LastSyncAt     time.Time
	State          string
}

type RemoteInfo struct {
	ID          string
	Alias       string
	Provider    string
	ProfileID   string
	Primary     bool
	Mirror      bool
	Enabled     bool
	SignedIn    bool
	RemoteLabel string
	AccountHint string
}

type LogoutOptions struct {
	Revoke       bool
	DeleteRemote bool
	ForceLocal   bool
}

type LogoutResult struct {
	Alias         string
	Provider      string
	NewPrimary    string
	RemoteDeleted bool
	TokenRevoked  bool
	LocalOnly     bool
}

type Diagnostic struct {
	Level   string
	Message string
}

type DiagnosticReport struct {
	Configured bool
	ProfileID  string
	Remotes    int
	Checks     []Diagnostic
}
