package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

func run() (int, error) {
	args := os.Args[1:]
	if len(args) >= 3 && args[0] == "child" {
		parent, err := strconv.Atoi(args[1])
		if err != nil {
			return 1, err
		}

		return 1, backup.WindowChild(parent, args[2:])
	}

	if len(args) >= 4 && args[0] == "run" && args[3] == "--" {
		return backup.RunBuffer(backup.WindowRuntime, args[1], append([]string{args[2]}, args[4:]...))
	}
	if len(args) == 2 && args[0] == "control" {
		return 0, backup.ControlWindow(backup.WindowRuntime, args[1])
	}

	return 1, fmt.Errorf("usage: backup-window run WINDOW COMMAND -- [ARGS...] | control WINDOW")
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}

	os.Exit(code)
}
