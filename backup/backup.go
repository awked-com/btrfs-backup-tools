package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Limits struct {
	MemoryBytes   uint64  `json:"memory_bytes"`
	Tasks         int     `json:"tasks"`
	WallSeconds   float64 `json:"wall_seconds"`
	ActiveSeconds float64 `json:"active_seconds"`
	IdleSeconds   float64 `json:"idle_seconds"`
}

type Policy struct {
	MinimumDays int `json:"minimum_days"`
	Days        int `json:"days"`
	Weeks       int `json:"weeks"`
	Months      int `json:"months"`
}

type Config struct {
	Root           string            `json:"root"`
	Directories    []string          `json:"directories"`
	Staging        string            `json:"staging"`
	CommandLock    string            `json:"command_lock"`
	ReceiveLock    string            `json:"receive_lock"`
	DirectoryLocks map[string]string `json:"directory_locks"`
	Limits         Limits            `json:"limits"`
	Retention      Policy            `json:"retention"`
	TransferWindow string            `json:"transfer_window"`
	Btrfs          string            `json:"btrfs"`
	SystemdRun     string            `json:"systemd_run"`
}

func LoadConfig(path string) (Config, error) {
	var config Config
	data, err := os.ReadFile(path)
	if err != nil {
		return config, err
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&config); err != nil {
		return config, err
	}

	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return config, err
	}

	required := []string{"root", "directories", "directory_locks"}
	group := "limits"
	groupFields := []string{"memory_bytes", "tasks", "wall_seconds", "active_seconds", "idle_seconds"}
	if _, retention := fields["retention"]; retention {
		group = "retention"
		groupFields = []string{"minimum_days", "days", "weeks", "months"}
	} else {
		required = append(required, "staging", "command_lock", "receive_lock", "btrfs", "systemd_run")
		if _, ok := fields["transfer_window"]; !ok {
			return config, fmt.Errorf("missing configuration field: transfer_window")
		}
	}

	required = append(required, group)
	if err = requireFields(fields, required); err != nil {
		return config, err
	}

	var nested map[string]json.RawMessage
	if err = json.Unmarshal(fields[group], &nested); err != nil {
		return config, err
	}

	if err = requireFields(nested, groupFields); err != nil {
		return config, fmt.Errorf("%s: %w", group, err)
	}

	if group == "retention" {
		p := config.Retention
		if p.MinimumDays < 0 || p.Days < 0 || p.Weeks < 0 || p.Months < 0 {
			return config, fmt.Errorf("retention periods must be nonnegative")
		}
	} else if l := config.Limits; l.MemoryBytes == 0 || l.Tasks <= 0 || l.WallSeconds <= 0 || l.ActiveSeconds <= 0 || l.IdleSeconds <= 0 {
		return config, fmt.Errorf("receive limits must be positive")
	}

	return config, nil
}

func requireFields(fields map[string]json.RawMessage, names []string) error {
	for _, name := range names {
		value, ok := fields[name]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("missing configuration field: %s", name)
		}
	}

	return nil
}

type SubvolumeInfo struct {
	ID           uint64
	UUID         [16]byte
	ReceivedUUID [16]byte
	ReceivedAt   time.Time
}

type Backend interface {
	IsSubvolume(string) (bool, error)
	SubvolumeInfo(string) (SubvolumeInfo, error)
	ReadOnly(string) (bool, error)
	HasDescendants(string) (bool, error)
	CreateSnapshot(string, string) error
	DeleteSubvolume(string) error
}

type Btrfs struct{}

func IsMount(path string) (bool, error) {
	current, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !current.IsDir() {
		return false, nil
	}

	parent, err := os.Stat(filepath.Join(path, ".."))
	if err != nil {
		return false, err
	}

	a, b := current.Sys().(*syscall.Stat_t), parent.Sys().(*syscall.Stat_t)
	return a.Dev != b.Dev || a.Ino == b.Ino, nil
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func canonicalDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("directory must be an absolute canonical path: %s", path)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if resolved != path || !info.IsDir() {
		return fmt.Errorf("directory must exist without symlink components: %s", path)
	}

	return nil
}
