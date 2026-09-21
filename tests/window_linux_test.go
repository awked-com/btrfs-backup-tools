//go:build linux

package storage_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/awked-com/btrfs-backup-tools/backup"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) {
	if len(os.Args) >= 4 && os.Args[1] == "child" {
		parent, err := strconv.Atoi(os.Args[2])
		if err == nil {
			err = backup.WindowChild(parent, os.Args[3:])
		}

		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func TestWindowGatesBufferUntilResume(t *testing.T) {
	root := t.TempDir()
	gates := filepath.Join(root, "gates")
	if err := os.Mkdir(gates, 0700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	window, opened, output := filepath.Join(root, "window"), filepath.Join(root, "opened"), filepath.Join(root, "output")
	if err := os.WriteFile(window, []byte("#!/bin/sh\ntest -e '"+opened+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		code, err := backup.RunBuffer(root, window, []string{"/bin/sh", "-c", `printf finished > "$1"`, "buffer-test", output})
		if err == nil && code != 0 {
			err = fmt.Errorf("buffer exited %d", code)
		}

		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := os.ReadDir(gates)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("buffer was not registered")
		}

		time.Sleep(10 * time.Millisecond)
	}

	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("buffer ran outside its window")
	}

	if err := os.WriteFile(opened, nil, 0600); err != nil {
		t.Fatal(err)
	}

	if err := backup.ControlWindow(root, window); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("buffer did not resume")
	}

	if data, err := os.ReadFile(output); err != nil || string(data) != "finished" {
		t.Fatalf("buffer failed: %q, %v", data, err)
	}

	if entries, _ := os.ReadDir(gates); len(entries) != 0 {
		t.Fatal("completed buffer gate survived")
	}
}

func TestStaleGateNeverSignalsReusedProcess(t *testing.T) {
	root := t.TempDir()
	gate := filepath.Join(root, "gate")
	start, _, err := backup.ProcessIdentity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}

	if err = os.WriteFile(gate, []byte(fmt.Sprintf("%d %d\n", os.Getpid(), start+1)), 0600); err != nil {
		t.Fatal(err)
	}

	if err = backup.SignalBuffer(gate, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}

	if _, err = os.Stat(gate); !os.IsNotExist(err) {
		t.Fatal("stale gate survived")
	}
}
