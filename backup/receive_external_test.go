package backup_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awked-com/btrfs-backup-tools/backup"
	"golang.org/x/sys/unix"
)

func TestReceiveDeadlinesPauseActiveAndIdleButNotWall(t *testing.T) {
	limits := backup.Limits{
		WallSeconds:   100,
		ActiveSeconds: 10,
		IdleSeconds:   5,
	}
	start := time.Now()
	deadline := backup.NewReceiveDeadline(limits, start)
	for _, elapsed := range []int{10, 50, 99} {
		if err := deadline.Update(start.Add(time.Duration(elapsed)*time.Second), false, false); err != nil {
			t.Fatal(err)
		}
	}

	if err := deadline.Update(start.Add(100*time.Second), false, false); err == nil || !strings.Contains(err.Error(), "wall-clock") {
		t.Fatalf("expected wall deadline, got %v", err)
	}

	deadline = backup.NewReceiveDeadline(limits, start)
	if err := deadline.Update(start.Add(4*time.Second), true, true); err != nil {
		t.Fatal(err)
	}

	if err := deadline.Update(start.Add(8*time.Second), true, true); err != nil {
		t.Fatal(err)
	}

	if err := deadline.Update(start.Add(10*time.Second), true, true); err == nil || !strings.Contains(err.Error(), "active") {
		t.Fatalf("progress bypassed active deadline: %v", err)
	}

	deadline = backup.NewReceiveDeadline(limits, start)
	if err := deadline.Update(start.Add(5*time.Second), true, false); err == nil || !strings.Contains(err.Error(), "idle") {
		t.Fatalf("expected idle deadline, got %v", err)
	}
}

func TestPublishRejectsNestedOrIncompleteReplicas(t *testing.T) {
	for _, kind := range []string{"nested", "writable", "unreceived", "symlink", "multiple", "wrong-name"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			stage, target := filepath.Join(root, "stage"), filepath.Join(root, "mail")
			for _, dir := range []string{stage, target} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}

			name := "mail.20260918"
			if kind == "wrong-name" {
				name = "other.20260918"
			}

			path := filepath.Join(stage, name)
			if kind == "symlink" {
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}

			if kind == "multiple" {
				if err := os.Mkdir(filepath.Join(stage, "mail.20260917"), 0700); err != nil {
					t.Fatal(err)
				}
			}

			info := backup.SubvolumeInfo{
				ID:           1,
				UUID:         [16]byte{1},
				ReceivedUUID: [16]byte{2},
				ReceivedAt:   now,
			}
			if kind == "unreceived" {
				info.ReceivedUUID = [16]byte{}
			}

			f := &fakeBtrfs{
				entries:  map[string]backup.SubvolumeInfo{path: info},
				writable: map[string]bool{path: kind == "writable"},
				nested:   kind == "nested",
			}
			if err := backup.PublishReplica(stage, target, f); err == nil {
				t.Fatal("unsafe replica published")
			}

			if entries, _ := os.ReadDir(target); len(entries) != 0 {
				t.Fatal("destination changed")
			}
		})
	}
}

func TestPublishCompletedReplica(t *testing.T) {
	root := t.TempDir()
	stage, target := filepath.Join(root, "stage"), filepath.Join(root, "mail")
	for _, dir := range []string{stage, target} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	path := filepath.Join(stage, "mail.20260918")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}

	info := backup.SubvolumeInfo{
		ID:           1,
		UUID:         [16]byte{1},
		ReceivedUUID: [16]byte{2},
		ReceivedAt:   now,
	}
	f := &fakeBtrfs{entries: map[string]backup.SubvolumeInfo{path: info}}
	if err := backup.PublishReplica(stage, target, f); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(target, filepath.Base(path))); err != nil {
		t.Fatal(err)
	}

	if entries, _ := os.ReadDir(stage); len(entries) != 0 {
		t.Fatal("staging copy survived publication")
	}
}

func TestSupervisionTransfersBytesAndRestoresInputFlags(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "stream")
	target := filepath.Join(root, "received")
	want := bytes.Repeat([]byte("a receive stream\n"), 20000)
	if err := os.WriteFile(source, want, 0600); err != nil {
		t.Fatal(err)
	}

	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()

	fd := int(input.Fd())
	before, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}

	config := backup.Config{
		Limits: backup.Limits{
			WallSeconds:   10,
			ActiveSeconds: 10,
			IdleSeconds:   5,
		},
	}
	if err = backup.SuperviseReceive([]string{"/bin/sh", "-c", `cat > "$1"`, "receive-test", target}, config, fd); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("stream bytes changed")
	}

	after, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("input flags were not restored")
	}
}

func TestPendingReceiveExpiresInsteadOfHanging(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()

	config := backup.Config{
		Limits: backup.Limits{
			WallSeconds:   2,
			ActiveSeconds: 1,
			IdleSeconds:   0.1,
		},
	}
	start := time.Now()
	err = backup.SuperviseReceive([]string{"/bin/sh", "-c", "sleep 30"}, config, int(reader.Fd()))
	if err == nil || !strings.Contains(err.Error(), "idle") {
		t.Fatalf("expected idle failure, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("stalled receive did not terminate promptly")
	}
}
