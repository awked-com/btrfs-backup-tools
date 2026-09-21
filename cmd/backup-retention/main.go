package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

func run() error {
	path := flag.String("config", "", "retention configuration")
	apply := flag.Bool("apply", false, "delete expired replicas; default is dry run")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		return fmt.Errorf("usage: backup-retention --config PATH [--apply]")
	}

	config, err := backup.LoadConfig(*path)
	if err != nil {
		return err
	}

	return backup.RunRetention(config, *apply, os.Stdout)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
