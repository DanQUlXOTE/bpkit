// Package profiles stores named Bindplane connections (server, API key and
// optional project) in a local file, so bpkit can work across several
// organizations and projects — and so one extension can hold two connections
// at once.
package profiles

import (
	"bpkit/internal/bpclient"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Profile struct {
	Name   string `json:"name"`
	Server string `json:"server"`
	APIKey string `json:"apiKey"`
	// Project scopes requests to one Bindplane project, sent as the
	// X-Bindplane-Account-ID header. A key scoped to several projects
	// requires it; a key that carries its own project does not.
	Project string `json:"project,omitempty"`
	// Proxy and CACert belong to the profile rather than the environment
	// so two connections can differ: the migrator holds a source and a
	// destination at once, and only one of them may sit behind a proxy.
	Proxy   string `json:"proxy,omitempty"`
	CACert  string `json:"caCert,omitempty"`
	SkipTLS bool   `json:"skipTLSVerify,omitempty"`
}

// ClientOptions is how to connect with this profile. Every caller that
// opens a connection goes through here, so a field added to Profile cannot
// be forgotten at one of them.
func (p Profile) ClientOptions() bpclient.Options {
	return bpclient.Options{
		BaseURL:       p.Server,
		APIKey:        p.APIKey,
		Project:       p.Project,
		Proxy:         p.Proxy,
		CACert:        p.CACert,
		SkipTLSVerify: p.SkipTLS,
	}
}

// Connect opens a connection with this profile.
func (p Profile) Connect() (*bpclient.Client, error) {
	return bpclient.New(p.ClientOptions())
}

// Title is the profile as shown in a list: its name, plus the project when
// one is set, since two profiles often differ only by project.
func (p Profile) Title() string {
	if p.Project != "" {
		return p.Name + " (" + p.Project + ")"
	}
	return p.Name
}

type Store struct {
	path string
}

// DefaultPath is $BPKIT_CONFIG, or <user config dir>/bpkit/profiles.json.
func DefaultPath() (string, error) {
	if p := os.Getenv("BPKIT_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bpkit", "profiles.json"), nil
}

// legacyPath is where bp_labeler kept the same file.
func legacyPath() (string, error) {
	if p := os.Getenv("BPLABELER_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "bplabeler", "profiles.json"), nil
}

// MigrateLegacy copies bp_labeler's profiles to bpkit's location the first
// time bpkit runs, so existing profiles survive the rename. It leaves the old
// file in place and does nothing once the new one exists. The returned bool
// reports whether anything was copied.
func MigrateLegacy() (bool, error) {
	dst, err := DefaultPath()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dst); err == nil {
		return false, nil // already have our own
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	src, err := legacyPath()
	if err != nil || src == dst {
		return false, nil
	}
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return false, fmt.Errorf("%s: %w", src, err)
	}
	if len(f.Profiles) == 0 {
		return false, nil
	}
	return true, NewStore(dst).write(f)
}

func NewStore(path string) *Store { return &Store{path: path} }

func (s *Store) Path() string { return s.path }

type file struct {
	Default  string    `json:"default,omitempty"`
	Profiles []Profile `json:"profiles"`
}

func (s *Store) load() (file, error) {
	var f file
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("%s: %w", s.path, err)
	}
	sort.Slice(f.Profiles, func(i, j int) bool {
		return strings.ToLower(f.Profiles[i].Name) < strings.ToLower(f.Profiles[j].Name)
	})
	return f, nil
}

// List returns the profiles sorted by name. A missing file is an empty list.
func (s *Store) List() ([]Profile, error) {
	f, err := s.load()
	return f.Profiles, err
}

// Default returns the name of the default profile, or "" when none is set.
func (s *Store) Default() (string, error) {
	f, err := s.load()
	return f.Default, err
}

// SetDefault marks name as the profile used when none is chosen. An empty
// name clears the default.
func (s *Store) SetDefault(name string) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	f.Default = ""
	if name != "" {
		found := false
		for _, p := range f.Profiles {
			if strings.EqualFold(p.Name, name) {
				f.Default, found = p.Name, true
			}
		}
		if !found {
			return fmt.Errorf("no profile named %q", name)
		}
	}
	return s.write(f)
}

func (s *Store) Get(name string) (Profile, error) {
	ps, err := s.List()
	if err != nil {
		return Profile{}, err
	}
	for _, p := range ps {
		if strings.EqualFold(p.Name, name) {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("no profile named %q", name)
}

func validate(p Profile) error {
	switch {
	case strings.TrimSpace(p.Name) == "":
		return errors.New("profile name is required")
	case strings.TrimSpace(p.Server) == "":
		return errors.New("server URL is required")
	case strings.TrimSpace(p.APIKey) == "":
		return errors.New("API key is required")
	}
	return nil
}

// Save adds a profile, or replaces the one called oldName (use "" to add).
// Renaming is allowed; names must stay unique, ignoring case.
func (s *Store) Save(oldName string, p Profile) error {
	p.Name, p.Server = strings.TrimSpace(p.Name), strings.TrimSpace(p.Server)
	p.APIKey, p.Project = strings.TrimSpace(p.APIKey), strings.TrimSpace(p.Project)
	p.Proxy, p.CACert = strings.TrimSpace(p.Proxy), strings.TrimSpace(p.CACert)
	if err := validate(p); err != nil {
		return err
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	ps := f.Profiles
	var out []Profile
	replaced := false
	for _, e := range ps {
		switch {
		case oldName != "" && strings.EqualFold(e.Name, oldName):
			out = append(out, p)
			replaced = true
		case strings.EqualFold(e.Name, p.Name):
			return fmt.Errorf("a profile named %q already exists", e.Name)
		default:
			out = append(out, e)
		}
	}
	if oldName != "" && !replaced {
		return fmt.Errorf("no profile named %q", oldName)
	}
	if oldName == "" {
		out = append(out, p)
	}
	if oldName != "" && strings.EqualFold(f.Default, oldName) {
		f.Default = p.Name // keep the default through a rename
	}
	f.Profiles = out
	return s.write(f)
}

func (s *Store) Delete(name string) error {
	f, err := s.load()
	if err != nil {
		return err
	}
	var out []Profile
	for _, e := range f.Profiles {
		if !strings.EqualFold(e.Name, name) {
			out = append(out, e)
		}
	}
	if len(out) == len(f.Profiles) {
		return fmt.Errorf("no profile named %q", name)
	}
	if strings.EqualFold(f.Default, name) {
		f.Default = ""
	}
	f.Profiles = out
	return s.write(f)
}

// write replaces the file atomically; it holds API keys so it is 0600.
func (s *Store) write(f file) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".profiles-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

// Mask shows only the tail of an API key.
func Mask(key string) string {
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return "****" + key[len(key)-4:]
}
