package storage_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

var now = date("2026-09-18T12:00")
var policy = backup.Policy{
	MinimumDays: 2,
	Days:        14,
	Weeks:       8,
	Months:      12,
}

func date(value string) time.Time {
	layout := "2006-01-02"
	if strings.Contains(value, "T") {
		layout = "2006-01-02T15:04"
	}

	parsed, err := time.ParseInLocation(layout, value, time.Local)
	if err != nil {
		panic(err)
	}

	return parsed
}

func copyAt(name, snapshot, received string) backup.Backup {
	if received == "" {
		received = snapshot
	}

	uuid := [16]byte{}
	copy(uuid[:], name)
	return backup.Backup{
		Path:         name,
		SnapshotTime: date(snapshot),
		ReceivedAt:   date(received),
		SubvolumeID:  1,
		UUID:         uuid,
	}
}

func assertKept(t *testing.T, copies []backup.Backup, p backup.Policy, names ...string) {
	t.Helper()
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}

	if got := backup.RetainedBackups(copies, now, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("kept %v, want %v", got, want)
	}
}

func TestLatestReceivedSurvivesExpiredTiers(t *testing.T) {
	assertKept(t, []backup.Backup{copyAt("latest", "2019-01-01", "2020-03-01"), copyAt("older", "2020-01-01", "2020-02-01")}, policy, "latest")
}

func TestBackdatedArrivalDoesNotDisplaceExistingDailyCopy(t *testing.T) {
	assertKept(t, []backup.Backup{
		copyAt("backdated", "2026-09-10T01:00", "2026-09-15T12:00"),
		copyAt("newest", "2026-09-18T10:00", ""),
		copyAt("original", "2026-09-10T12:00", "2026-09-10T13:00"),
	}, policy, "original", "newest")
}

func TestRecentReceiptsAndFutureSnapshotsSurvive(t *testing.T) {
	assertKept(t, []backup.Backup{
		copyAt("recent", "2020-01-01", "2026-09-16"),
		copyAt("expired", "2020-01-02", "2026-09-15"),
		copyAt("future", "2026-09-19", "2026-09-10"),
		copyAt("newest", "2026-09-18T10:00", ""),
	}, policy, "recent", "future", "newest")
}

func TestRetentionBoundariesInclusive(t *testing.T) {
	for _, test := range []struct {
		name, inside, outside string
		p                     backup.Policy
	}{
		{"daily", "2026-09-04", "2026-09-03", backup.Policy{Days: 14}},
		{"weekly", "2026-07-19", "2026-07-12", backup.Policy{Weeks: 8}},
		{"monthly", "2025-09-07", "2025-08-03", backup.Policy{Months: 12}},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertKept(t, []backup.Backup{
				copyAt("inside", test.inside, ""),
				copyAt("outside", test.outside, ""),
				copyAt("newest", "2026-09-18T10:00", ""),
			}, test.p, "inside", "newest")
		})
	}
}

type fakeBtrfs struct {
	entries  map[string]backup.SubvolumeInfo
	changed  map[string]backup.SubvolumeInfo
	writable map[string]bool
	nested   bool
}

func (f *fakeBtrfs) IsSubvolume(path string) (bool, error) {
	_, ok := f.entries[path]
	return ok, nil
}

func (f *fakeBtrfs) SubvolumeInfo(path string) (backup.SubvolumeInfo, error) {
	info, ok := f.entries[path]
	if !ok {
		return info, fmt.Errorf("missing subvolume %s", path)
	}

	if replacement, ok := f.changed[path]; ok {
		f.entries[path] = replacement
		delete(f.changed, path)
	}

	return info, nil
}

func (f *fakeBtrfs) ReadOnly(path string) (bool, error) { return !f.writable[path], nil }

func (f *fakeBtrfs) HasDescendants(string) (bool, error) { return f.nested, nil }

func (f *fakeBtrfs) CreateSnapshot(source, target string) error {
	if err := os.Mkdir(target, 0700); err != nil {
		return err
	}

	f.entries[target] = f.entries[source]
	return nil
}

func (f *fakeBtrfs) DeleteSubvolume(path string) error {
	if f.nested {
		return fmt.Errorf("nested subvolume")
	}

	return os.Remove(path)
}

func fixture(t *testing.T) (string, *fakeBtrfs) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "mail")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}

	f := &fakeBtrfs{
		entries:  map[string]backup.SubvolumeInfo{},
		changed:  map[string]backup.SubvolumeInfo{},
		writable: map[string]bool{},
	}
	for i, name := range []string{"mail.20200101", "mail.20260918", "mail.20190101"} {
		path := filepath.Join(directory, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}

		timestamp, _ := backup.SnapshotTime(name, "mail")
		info := backup.SubvolumeInfo{
			ID:           uint64(i + 1),
			UUID:         [16]byte{byte(i + 1)},
			ReceivedUUID: [16]byte{1},
			ReceivedAt:   timestamp,
		}
		if i == 2 {
			info.ReceivedUUID = [16]byte{}
		}

		f.entries[path] = info
	}

	if err := os.Symlink(filepath.Join(directory, "mail.20200101"), filepath.Join(directory, "mail.20180101")); err != nil {
		t.Fatal(err)
	}

	return directory, f
}

func TestDryRunAndApplyPreserveIncompleteCopiesAndSymlinks(t *testing.T) {
	directory, f := fixture(t)
	var output bytes.Buffer
	if err := backup.PruneDirectory(directory, f, now, policy, false, &output); err != nil {
		t.Fatal(err)
	}

	expired := filepath.Join(directory, "mail.20200101")
	if !strings.Contains(output.String(), "Would delete "+expired) {
		t.Fatal(output.String())
	}

	if entries, _ := os.ReadDir(directory); len(entries) != 4 {
		t.Fatal("dry run removed files")
	}

	if err := backup.PruneDirectory(directory, f, now, policy, true, &output); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired copy survived: %v", err)
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}

	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	if !reflect.DeepEqual(names, []string{"mail.20180101", "mail.20190101", "mail.20260918"}) {
		t.Fatal(names)
	}
}

func TestChangedReplicaNeverDeleted(t *testing.T) {
	for _, change := range []string{"uuid", "id", "received", "writable"} {
		t.Run(change, func(t *testing.T) {
			directory, f := fixture(t)
			path := filepath.Join(directory, "mail.20200101")
			info := f.entries[path]
			switch change {
			case "uuid":
				info.UUID[0]++
			case "id":
				info.ID++
			case "received":
				info.ReceivedAt = info.ReceivedAt.Add(time.Second)
			}

			f.changed[path] = info
			backend := backup.Backend(f)
			if change == "writable" {
				backend = &becomesWritable{
					fakeBtrfs: f,
					path:      path,
				}
			}

			err := backup.PruneDirectory(directory, backend, now, policy, true, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), "changed during retention") {
				t.Fatalf("expected changed replica failure, got %v", err)
			}

			if _, err = os.Stat(path); err != nil {
				t.Fatal("changed replica was deleted")
			}
		})
	}
}

type becomesWritable struct {
	*fakeBtrfs
	path string
	read bool
}

func (f *becomesWritable) ReadOnly(path string) (bool, error) {
	if path == f.path {
		wasRead := f.read
		f.read = true
		return !wasRead, nil
	}

	return true, nil
}

func TestSnapshotNames(t *testing.T) {
	for _, name := range []string{"mail.20260918", "mail.20260918T1234_1", "mail.20260918T123456+0200"} {
		if _, ok := backup.SnapshotTime(name, "mail"); !ok {
			t.Errorf("valid name rejected: %s", name)
		}
	}

	for _, name := range []string{
		"other.20260918",
		"mail.20260230",
		"mail.19691231",
		"mail.20260918/evil",
		"mail.20260918T123456+2500",
		"mail.20260918T123456+2400",
		"mail.20260918T123456+0060",
	} {
		if _, ok := backup.SnapshotTime(name, "mail"); ok {
			t.Errorf("invalid name accepted: %s", name)
		}
	}
}

func TestRetentionRequiresCompletePolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retention.json")
	for _, missing := range []string{"minimum_days", "days", "weeks", "months"} {
		p := map[string]int{
			"minimum_days": 2,
			"days":         14,
			"weeks":        8,
			"months":       12,
		}
		delete(p, missing)
		data, err := json.Marshal(map[string]any{
			"root":            "/backup",
			"directories":     []string{},
			"directory_locks": map[string]string{},
			"retention":       p,
		})
		if err != nil {
			t.Fatal(err)
		}

		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}

		if _, err = backup.LoadConfig(path); err == nil {
			t.Fatalf("missing %s silently shortened retention", missing)
		}
	}
}
