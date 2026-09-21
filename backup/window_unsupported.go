//go:build !linux

package backup

import "errors"

const WindowRuntime = "/run/btrfs-backup-window"

func WindowChild(int, []string) error { return errors.New("backup window control requires Linux") }

func RunBuffer(string, string, []string) (int, error) {
	return 1, errors.New("backup window control requires Linux")
}

func ControlWindow(string, string) error { return errors.New("backup window control requires Linux") }
