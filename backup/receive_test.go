package backup

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReceiveScopeRuntimeUsesDecimalSeconds(t *testing.T) {
	for _, test := range []struct {
		name    string
		seconds float64
		want    string
	}{
		{"thirty days", 30 * 24 * 60 * 60, "2592000"},
		{"fractional seconds", 0.125, "0.125"},
		{"long fractional duration", 2592000.125, "2592000.125"},
		{"microsecond", 0.000001, "0.000001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Config{Limits: Limits{WallSeconds: test.seconds}}
			args := receiveScopeArgs("/bin/backup-confined-receive", "/etc/backup.json", "/backups/target", config)
			if want := "--property=RuntimeMaxSec=" + test.want; !slices.Contains(args, want) {
				t.Fatalf("scope arguments %q do not contain %q", args, want)
			}
		})
	}
}

func TestFailedReceiveRetainsUnlockedPrivateLease(t *testing.T) {
	root := t.TempDir()
	staging, target := filepath.Join(root, ".incoming"), filepath.Join(root, "target")
	for _, dir := range []string{staging, target} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	receiver := filepath.Join(root, "receiver")
	if err := os.WriteFile(receiver, []byte("#!/bin/sh\nexit 42\n"), 0700); err != nil {
		t.Fatal(err)
	}

	config := Config{
		Staging: staging,
		Root:    root,
		Btrfs:   receiver,
		Limits: Limits{
			WallSeconds:   10,
			ActiveSeconds: 10,
			IdleSeconds:   5,
		},
	}
	if err := receiveStaged(target, config, nil); err == nil {
		t.Fatal("failed stream succeeded")
	}

	leases, err := filepath.Glob(filepath.Join(staging, "*.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 {
		t.Fatalf("expected retained lease, got %v", leases)
	}

	lease, err := os.Open(leases[0])
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()

	info, err := lease.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("lease mode %v", info.Mode())
	}

	if err = unix.Flock(int(lease.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatalf("failed receive still holds lease: %v", err)
	}

	if info, err = os.Stat(strings.TrimSuffix(leases[0], ".lock")); err != nil || !info.IsDir() {
		t.Fatalf("staging was not retained: %v", err)
	}
}
