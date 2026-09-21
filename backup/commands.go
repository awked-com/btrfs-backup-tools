package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/shlex"
	"golang.org/x/sys/unix"
)

func commandWords(command string, pipes bool) ([]string, error) {
	// shlex treats # as a comment; SSH commands use literal # and optional pipes.
	var prepared strings.Builder
	var quote rune
	escaped := false
	for _, r := range command {
		if escaped {
			prepared.WriteRune(r)
			escaped = false
			continue
		}

		if r == '\\' && quote != '\'' {
			prepared.WriteRune(r)
			escaped = true
			continue
		}

		if quote != 0 {
			if r == quote {
				quote = 0
			}

			prepared.WriteRune(r)
			continue
		}

		if r == '\'' || r == '"' {
			quote = r
		}

		if r == '#' {
			prepared.WriteRune('\\')
		}

		if r == '|' && pipes {
			prepared.WriteString(" | ")
		} else {
			prepared.WriteRune(r)
		}
	}

	return shlex.Split(prepared.String())
}

func quoteWords(words []string) string {
	quoted := make([]string, len(words))
	for i, word := range words {
		quoted[i] = "'" + strings.ReplaceAll(word, "'", "'\"'\"'") + "'"
	}

	return strings.Join(quoted, " ")
}

func InfoCommand(command string, config Config) ([]string, error) {
	args, err := commandWords(command, false)
	if err != nil {
		return nil, err
	}
	if len(args) < 5 || !slices.Equal(args[:2], []string{"sudo", "-n"}) {
		return nil, errors.New("expected a privileged metadata query")
	}

	args = args[2:]
	rawPath := args[len(args)-1]
	for _, component := range strings.Split(rawPath, string(os.PathSeparator)) {
		if component == ".." {
			return nil, errors.New("metadata path is outside the assigned backup directories")
		}
	}

	path := filepath.Clean(rawPath)
	allowed := false
	for _, directory := range config.Directories {
		if within(directory, path) || filepath.Dir(path) == directory {
			allowed = true
		}
	}

	if !allowed || !within(path, config.Root) {
		return nil, errors.New("metadata path is outside the assigned backup directories")
	}

	if err := canonicalDirectory(path); err != nil {
		return nil, err
	}

	valid := false
	switch {
	case len(args) >= 4 && slices.Equal(args[:3], []string{"btrfs", "subvolume", "list"}):
		valid = true
		for _, option := range args[3 : len(args)-1] {
			if !slices.Contains([]string{"-a", "-c", "-u", "-q", "-R", "-r", "-o", "-d"}, option) {
				valid = false
			}
		}
	case len(args) >= 4 && slices.Equal(args[:3], []string{"btrfs", "subvolume", "show"}):
		options := args[3 : len(args)-1]
		valid = len(options) == 0 || slices.Equal(options, []string{"--rootid=5"})
	case len(args) == 4 && slices.Equal(args[:3], []string{"btrfs", "filesystem", "usage"}):
		valid = true
	case slices.Equal(args[:len(args)-1], []string{"readlink", "-v", "-e"}):
		valid = true
	case slices.Equal(args[:len(args)-1], []string{"test", "-d"}):
		valid = true
	}

	if !valid {
		return nil, errors.New("unsupported metadata command or options")
	}

	args[len(args)-1] = path
	if args[0] == "btrfs" {
		if !filepath.IsAbs(config.Btrfs) {
			return nil, errors.New("metadata executable must be an absolute path")
		}
		args[0] = config.Btrfs
	}
	return args, nil
}

func RunInfo(command string, config Config, output io.Writer) error {
	args, err := InfoCommand(command, config)
	if err != nil {
		return err
	}
	// InfoCommand already requires an existing directory with no symlink components.
	switch args[0] {
	case "readlink":
		_, err = fmt.Fprintln(output, args[len(args)-1])
		return err
	case "test":
		return nil
	default:
		return syscall.Exec(args[0], args, os.Environ())
	}
}

func CheckedSSHTarget(value string, targets []string) (string, error) {
	target := strings.TrimRight(value, "/")
	if !slices.Contains(targets, target) {
		return "", errors.New("write destination is not an assigned replica directory")
	}

	if err := canonicalDirectory(target); err != nil {
		return "", err
	}

	return target, nil
}

func PrepareCommand(command string, targets []string) ([][]string, bool, error) {
	words, err := commandWords(command, true)
	if err != nil {
		return nil, false, err
	}

	pipeline := [][]string{{}}
	for _, token := range words {
		if token == "|" {
			if len(pipeline[len(pipeline)-1]) == 0 {
				return nil, false, errors.New("empty pipeline command")
			}

			pipeline = append(pipeline, []string{})
		} else {
			pipeline[len(pipeline)-1] = append(pipeline[len(pipeline)-1], token)
		}
	}

	if len(pipeline[len(pipeline)-1]) == 0 {
		return nil, false, errors.New("empty command")
	}

	receiving := false
	for _, stage := range pipeline {
		program := stage
		privileged := len(stage) >= 2 && slices.Equal(stage[:2], []string{"sudo", "-n"})
		if privileged {
			program = stage[2:]
		}

		if len(program) == 0 {
			return nil, false, errors.New("empty privileged command")
		}

		if program[0] == "mkdir" {
			if len(pipeline) != 1 || len(program) != 3 || program[1] != "-p" {
				return nil, false, errors.New("only an existing replica directory may be requested")
			}

			_, err = CheckedSSHTarget(program[2], targets)
			return nil, false, err
		}

		if len(program) >= 2 && slices.Equal(program[:2], []string{"btrfs", "receive"}) {
			if !privileged || len(program) != 3 {
				return nil, false, errors.New("receive options are controlled by the receiver")
			}

			stage[len(stage)-1], err = CheckedSSHTarget(program[2], targets)
			if err != nil {
				return nil, false, err
			}

			receiving = true
		}
	}

	if len(pipeline) > 1 && (!receiving || len(pipeline) != 2 || !slices.Equal(pipeline[0], []string{"mbuffer", "-v", "1", "-q", "-m", "256m"}) || len(pipeline[1]) < 4 || !slices.Equal(pipeline[1][:4], []string{"sudo", "-n", "btrfs", "receive"})) {
		return nil, false, errors.New("only the configured receive buffer pipeline is permitted")
	}

	return pipeline, receiving, nil
}

func ReceiverLock(path string, exclusive bool) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	file := os.NewFile(uintptr(fd), path)
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err == nil {
		if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != 0 || info.Mode&0022 != 0 {
			err = errors.New("receiver lock must be a protected, root-owned regular file")
		}
	}

	if err == nil {
		mode := unix.LOCK_SH
		if exclusive {
			mode = unix.LOCK_EX
		}

		err = unix.Flock(fd, mode|unix.LOCK_NB)
	}

	if err != nil {
		file.Close()
		return nil, err
	}

	return file, nil
}

func RunReceive(bufferCommand, receiveCommand []string) (int, error) {
	buffer := exec.Command(bufferCommand[0], bufferCommand[1:]...)
	buffer.Stdin = os.Stdin
	buffer.Stderr = os.Stderr
	buffer.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stream, err := buffer.StdoutPipe()
	if err != nil {
		return 1, err
	}

	if err = buffer.Start(); err != nil {
		return 1, err
	}

	receiver := exec.Command(receiveCommand[0], receiveCommand[1:]...)
	receiver.Stdin = stream
	receiver.Stdout = os.Stdout
	receiver.Stderr = os.Stderr
	defer func() {
		stream.Close()
		_ = unix.Kill(-buffer.Process.Pid, unix.SIGKILL)
		_ = buffer.Wait()
	}()

	if err = receiver.Start(); err != nil {
		return 1, err
	}

	stream.Close()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, unix.SIGHUP, unix.SIGTERM)
	defer signal.Stop(signals)

	done := make(chan error, 1)
	go func() { done <- receiver.Wait() }()
	select {
	case err = <-done:
		return commandStatus(err)
	case signum := <-signals:
		_ = unix.Kill(-buffer.Process.Pid, unix.SIGKILL)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}

		return 128 + int(signum.(syscall.Signal)), nil
	}
}

func commandStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return waitStatus(exit.Sys().(syscall.WaitStatus)), nil
	}

	return 1, err
}

func waitStatus(status syscall.WaitStatus) int {
	if status.Signaled() {
		return 128 + int(status.Signal())
	}

	return status.ExitStatus()
}

func RunSSHFilter(config Config, info, receive, buffer, limiter, window, sudo string) (int, error) {
	pipeline, receiving, err := PrepareCommand(os.Getenv("SSH_ORIGINAL_COMMAND"), config.Directories)
	if err != nil {
		return 255, err
	}
	if pipeline == nil {
		return 0, nil
	}

	if receiving {
		lock, err := ReceiverLock(config.CommandLock, true)
		if err != nil {
			return 255, err
		}
		defer lock.Close()

		if config.Limits.MemoryBytes == 0 {
			return 255, errors.New("receive memory limit is required")
		}

		self, err := os.Executable()
		if err != nil {
			return 255, err
		}

		bufferCommand := []string{
			limiter,
			fmt.Sprintf("--as=%d:%d", config.Limits.MemoryBytes, config.Limits.MemoryBytes),
			"--", buffer,
			"-v", "1",
			"-q",
			"-m", "256m",
		}
		if window != "" {
			// The Go window supervisor must start before applying mbuffer's address-space limit.
			bufferCommand = append([]string{filepath.Join(filepath.Dir(self), "backup-window"), "run", window, limiter, "--"}, bufferCommand[1:]...)
		}

		return RunReceive(bufferCommand, []string{
			sudo,
			"-n",
			receive,
			pipeline[len(pipeline)-1][len(pipeline[len(pipeline)-1])-1],
		})
	}

	if slices.Equal(pipeline[0], []string{"cat", "/proc/self/mountinfo"}) || slices.Equal(pipeline[0], []string{"cat", "/proc/self/mounts"}) {
		file, err := os.Open(pipeline[0][1])
		if err != nil {
			return 255, err
		}
		defer file.Close()

		_, err = io.Copy(os.Stdout, file)
		return 0, err
	}

	args := []string{"sudo", "-n", info, quoteWords(pipeline[0])}
	return 255, syscall.Exec(sudo, args, os.Environ())
}
