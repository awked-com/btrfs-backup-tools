package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

func run() error {
	path := flag.String("config", "", "receiver configuration")
	confined := flag.Bool("confined", false, "run inside the receive scope")
	flag.Parse()
	if *path == "" || flag.NArg() != 1 {
		return fmt.Errorf("usage: backup-confined-receive --config PATH [--confined] TARGET")
	}

	config, err := backup.LoadConfig(*path)
	if err != nil {
		return err
	}

	return backup.RunConfinedReceive(*path, flag.Arg(0), config, *confined)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Backup receive rejected:", err)
		var interrupted *backup.InterruptedError
		if errors.As(err, &interrupted) {
			os.Exit(128 + int(interrupted.Signal))
		}

		os.Exit(1)
	}
}
