package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

func run() error {
	path := flag.String("config", "", "receiver configuration")
	flag.Parse()
	if *path == "" || flag.NArg() != 1 {
		return fmt.Errorf("usage: backup-info --config PATH COMMAND")
	}

	config, err := backup.LoadConfig(*path)
	if err != nil {
		return err
	}

	return backup.RunInfo(flag.Arg(0), config, os.Stdout)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Backup metadata query rejected:", err)
		os.Exit(255)
	}
}
