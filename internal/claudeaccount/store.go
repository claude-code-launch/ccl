// Package claudeaccount stores local Claude Code profile selections. Claude
// Code itself owns authentication, credentials, and token refresh.
package claudeaccount

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/claude-code-launch/ccl/internal/fsutil"
)

type Store struct{ root string }

type registry struct {
	Active   string            `json:"active,omitempty"`
	Accounts map[string]string `json:"accounts"`
}

type Candidate struct {
	Name string
	Path string
	New  bool
	id   string
}

func (c Candidate) ID() string { return c.id }

// Profile resolves only an existing locally managed profile, never an
// arbitrary filesystem path from a synced provider configuration.
func (s *Store) Profile(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid Claude account profile id")
	}
	path := s.profile(id)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("Claude account profile is unavailable on this device; sign in again with ccl oauth claude <name>")
	}
	return path, nil
}

// PrepareExisting keeps the stable profile id across provider renames and
// copies, including accounts that need to be signed in again on this device.
func (s *Store) PrepareExisting(name, id string) (Candidate, error) {
	if err := validateName(name); err != nil {
		return Candidate{}, err
	}
	if !validID(id) {
		return Candidate{}, fmt.Errorf("invalid Claude account profile id")
	}
	path := s.profile(id)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return Candidate{}, err
	}
	return Candidate{Name: name, Path: path, id: id}, nil
}

// ImportLegacy transfers the previous local account selector once. The
// registry and credentials remain untouched; the marker prevents a removed or
// renamed provider from being recreated on subsequent launches.
func (s *Store) ImportLegacy(ctx context.Context, importAccounts func(map[string]string, string) error) error {
	if _, err := os.Stat(filepath.Join(s.root, "accounts.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	unlock, err := fsutil.AcquireFileLock(ctx, filepath.Join(s.root, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	marker := filepath.Join(s.root, ".providers-imported")
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	r, err := s.read()
	if err != nil {
		return err
	}
	if err := importAccounts(r.Accounts, r.Active); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(marker, []byte("1\n"), 0o600)
}

func Open() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{root: filepath.Join(home, ".ccl", "claude-accounts")}, nil
}

func (s *Store) read() (registry, error) {
	r := registry{Accounts: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(s.root, "accounts.json"))
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	if len(data) > 1<<20 {
		return r, fmt.Errorf("Claude account registry is too large")
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, fmt.Errorf("read Claude account registry: %w", err)
	}
	if r.Accounts == nil {
		r.Accounts = make(map[string]string)
	}
	for name, id := range r.Accounts {
		if err := validateName(name); err != nil || !validID(id) {
			return r, fmt.Errorf("invalid Claude account registry entry %q", name)
		}
	}
	if r.Active != "" && r.Accounts[r.Active] == "" {
		return r, fmt.Errorf("selected Claude account %q is missing", r.Active)
	}
	return r, nil
}

func validateName(name string) error {
	if name == "" || len(name) > 128 || name != strings.TrimSpace(name) {
		return fmt.Errorf("account name must contain 1 to 128 bytes")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("account name cannot contain control characters")
		}
	}
	switch strings.ToLower(name) {
	case "default", "add", "list", "use", "help":
		return fmt.Errorf("account name %q is reserved", name)
	}
	return nil
}

func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

func (s *Store) profile(id string) string { return filepath.Join(s.root, "profiles", id) }

// Prepare reserves a private directory for a new login, or returns an existing
// profile for re-authentication. A new profile is registered only on success.
func (s *Store) Prepare(name string) (Candidate, error) {
	return s.prepare(name, true)
}

// PrepareNew creates a distinct profile without consulting legacy aliases.
func (s *Store) PrepareNew(name string) (Candidate, error) {
	return s.prepare(name, false)
}

func (s *Store) prepare(name string, reuseLegacy bool) (Candidate, error) {
	name = strings.TrimSpace(name)
	if name != "" {
		if err := validateName(name); err != nil {
			return Candidate{}, err
		}
	}
	var id string
	if reuseLegacy {
		r, err := s.read()
		if err != nil {
			return Candidate{}, err
		}
		id = r.Accounts[name]
	}
	if id != "" {
		path := s.profile(id)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return Candidate{}, err
		}
		return Candidate{Name: name, Path: path, id: id}, nil
	}
	if err := os.MkdirAll(filepath.Join(s.root, "profiles"), 0o700); err != nil {
		return Candidate{}, err
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return Candidate{}, err
	}
	id = hex.EncodeToString(raw[:])
	if name == "" {
		name = "claude-" + id[:8]
	}
	path := s.profile(id)
	if err := os.Mkdir(path, 0o700); err != nil {
		return Candidate{}, err
	}
	return Candidate{Name: name, Path: path, New: true, id: id}, nil
}

// PreferName uses an account's reported email as the generated alias when it
// is available. Existing aliases are never replaced by a new login.
func (s *Store) PreferName(c Candidate, name string) Candidate {
	name = strings.TrimSpace(name)
	if !c.New || validateName(name) != nil {
		return c
	}
	r, err := s.read()
	if err == nil && r.Accounts[name] == "" {
		c.Name = name
	}
	return c
}

func (s *Store) update(ctx context.Context, fn func(*registry) error) error {
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	unlock, err := fsutil.AcquireFileLock(ctx, filepath.Join(s.root, ".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	r, err := s.read()
	if err != nil {
		return err
	}
	if err := fn(&r); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(s.root, "accounts.json"), append(data, '\n'), 0o600)
}

// Activate records a successful login and selects it for default launches.
func (s *Store) Activate(ctx context.Context, c Candidate) error {
	if !validID(c.id) || c.Path != s.profile(c.id) || validateName(c.Name) != nil {
		return fmt.Errorf("invalid Claude account profile")
	}
	return s.update(ctx, func(r *registry) error {
		if old := r.Accounts[c.Name]; old != "" && old != c.id {
			return fmt.Errorf("account %q was created by another process; profile saved at %s", c.Name, c.Path)
		}
		r.Accounts[c.Name] = c.id
		r.Active = c.Name
		return nil
	})
}

// Select switches the locally managed default account. The reserved name
// "default" restores the user's original Claude Code configuration.
func (s *Store) Select(ctx context.Context, name string) error {
	return s.update(ctx, func(r *registry) error {
		if name == "default" {
			r.Active = ""
			return nil
		}
		id := r.Accounts[name]
		if id == "" {
			return fmt.Errorf("Claude account %q not found; use ccl oauth claude list", name)
		}
		if info, err := os.Stat(s.profile(id)); err != nil || !info.IsDir() {
			return fmt.Errorf("Claude account %q profile is unavailable", name)
		}
		r.Active = name
		return nil
	})
}

// Resolve returns an absolute profile path. An empty name uses the selected
// account; "default" selects Claude Code's original environment.
func (s *Store) Resolve(name string) (string, string, error) {
	r, err := s.read()
	if err != nil {
		return "", "", err
	}
	if name == "" {
		name = r.Active
	}
	if name == "" || name == "default" {
		return "", "", nil
	}
	id := r.Accounts[name]
	if id == "" {
		return "", "", fmt.Errorf("Claude account %q not found; use ccl oauth claude list", name)
	}
	path := s.profile(id)
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("Claude account %q profile is unavailable", name)
	}
	return name, path, nil
}

func (s *Store) List() ([]string, string, error) {
	r, err := s.read()
	if err != nil {
		return nil, "", err
	}
	names := make([]string, 0, len(r.Accounts))
	for name := range r.Accounts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, r.Active, nil
}
