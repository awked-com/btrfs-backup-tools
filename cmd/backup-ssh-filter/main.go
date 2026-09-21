package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"

	"github.com/awked-com/btrfs-backup-tools/backup"
)

func run() (int, error) {
	path := flag.String("config", "", "receiver configuration")
	info := flag.String("info", "", "metadata helper")
	receive := flag.String("receive", "", "receive helper")
	buffer := flag.String("buffer", "", "buffer executable")
	limiter := flag.String("limiter", "", "prlimit executable")
	window := flag.String("window", "", "buffer transfer window command")
	sudo := flag.String("sudo", "sudo", "sudo executable")
	flag.Parse()
	if *path == "" || *info == "" || *receive == "" || *buffer == "" || *limiter == "" || flag.NArg() != 0 {
		return 255, fmt.Errorf("usage: backup-ssh-filter --config PATH --info COMMAND --receive COMMAND --buffer COMMAND --limiter PRLIMIT [--window COMMAND] [--sudo COMMAND]")
	}

	config, err := backup.LoadConfig(*path)
	if err != nil {
		return 255, err
	}

	sudoPath, err := exec.LookPath(*sudo)
	if err != nil {
		return 255, err
	}
	return backup.RunSSHFilter(config, *info, *receive, *buffer, *limiter, *window, sudoPath)
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Backup command rejected:", err)
		code = 255
	}

	os.Exit(code)
}
