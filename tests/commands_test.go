package storage_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

func canonicalTemp(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	return root
}

func TestSSHReceiveCommandsAreConfined(t *testing.T) {
	root := canonicalTemp(t)
	target := filepath.Join(root, "mail")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{
		"sudo -n btrfs receive " + target,
		"mbuffer -v 1 -q -m 256m|sudo -n btrfs receive '" + target + "/'",
	} {
		pipeline, receiving, err := backup.PrepareCommand(command, []string{target})
		if err != nil || !receiving || pipeline[len(pipeline)-1][4] != target {
			t.Fatalf("valid receive rejected: %v %v", pipeline, err)
		}
	}

	for _, command := range []string{
		"btrfs receive " + target,
		"sudo -n btrfs receive --chroot " + target,
		"sudo -n btrfs receive " + root,
		"cat|sudo -n btrfs receive " + target,
		"mbuffer -v 1 -q -m 256m||sudo -n btrfs receive " + target,
		"sudo -n mkdir -p " + target + "|cat",
		"",
	} {
		if _, _, err := backup.PrepareCommand(command, []string{target}); err == nil {
			t.Errorf("unsafe command accepted: %s", command)
		}
	}

	pipeline, receiving, err := backup.PrepareCommand("mkdir -p '"+target+"'", []string{target})
	if err != nil || pipeline != nil || receiving {
		t.Fatalf("existing target mkdir rejected: %v", err)
	}

	alias := filepath.Join(root, "alias")
	if err = os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}

	if _, _, err = backup.PrepareCommand("sudo -n btrfs receive "+alias, []string{alias}); err == nil {
		t.Fatal("symlink destination accepted")
	}
}

func TestMetadataCannotDescendIntoReplicaContents(t *testing.T) {
	root := canonicalTemp(t)
	directory := filepath.Join(root, "sender", "mail")
	replica := filepath.Join(directory, "mail.20260918")
	contents := filepath.Join(replica, "contents")
	if err := os.MkdirAll(contents, 0700); err != nil {
		t.Fatal(err)
	}

	config := backup.Config{
		Root:        root,
		Directories: []string{directory},
		Btrfs:       "/trusted/btrfs",
	}
	for _, path := range []string{root, filepath.Dir(directory), directory, replica, replica + "/"} {
		args, err := backup.InfoCommand("sudo -n btrfs subvolume show '"+path+"'", config)
		if err != nil || args[0] != "/trusted/btrfs" {
			t.Fatalf("metadata query rejected for %s: %v", path, err)
		}
	}

	for _, command := range []string{
		"sudo -n btrfs subvolume show " + contents,
		"sudo -n btrfs subvolume delete " + replica,
		"sudo -n btrfs subvolume list --all " + root,
		"sudo -n test -d " + root + "/../",
		"sudo -n btrfs subvolume show " + replica + "; echo bad",
		"sudo -n readlink -f " + replica,
	} {
		if _, err := backup.InfoCommand(command, config); err == nil {
			t.Errorf("unsafe metadata command accepted: %s", command)
		}
	}

	alias := filepath.Join(directory, "alias")
	if err := os.Symlink(replica, alias); err != nil {
		t.Fatal(err)
	}

	if _, err := backup.InfoCommand("sudo -n test -d "+alias, config); err == nil {
		t.Fatal("metadata symlink accepted")
	}

	for _, command := range []string{"sudo -n readlink -v -e " + replica, "sudo -n test -d " + directory} {
		var output bytes.Buffer
		if err := backup.RunInfo(command, config, &output); err != nil {
			t.Fatal(err)
		}
		want := ""
		if strings.Contains(command, "readlink") {
			want = replica + "\n"
		}
		if output.String() != want {
			t.Fatalf("metadata output = %q, want %q", output.String(), want)
		}
	}
}

func TestProtectedLocksRejectWritableFilesAndSymlinks(t *testing.T) {
	root := canonicalTemp(t)
	path := filepath.Join(root, "lock")
	if err := os.WriteFile(path, []byte{}, 0600); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}

	if lock, err := backup.ReceiverLock(path, true); err == nil {
		lock.Close()
		t.Fatal("writable lock accepted")
	}

	alias := filepath.Join(root, "alias")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}

	if lock, err := backup.ReceiverLock(alias, true); err == nil {
		lock.Close()
		t.Fatal("symlink lock accepted")
	}

	if os.Getuid() != 0 {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}

		if lock, err := backup.ReceiverLock(path, true); err == nil {
			lock.Close()
			t.Fatal("non-root lock accepted")
		} else if !strings.Contains(err.Error(), "root-owned") {
			t.Fatal(err)
		}
	}
}
