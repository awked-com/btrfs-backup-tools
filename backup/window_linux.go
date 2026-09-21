//go:build linux

package backup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const WindowRuntime = "/run/btrfs-backup-window"

func ProcessIdentity(pid int) (uint64, uint32, error) {
	path := fmt.Sprintf("/proc/%d", pid)
	data, err := os.ReadFile(filepath.Join(path, "stat"))
	if err != nil {
		return 0, 0, err
	}

	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, 0, errors.New("invalid process stat")
	}

	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 {
		return 0, 0, errors.New("invalid process stat")
	}

	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, err
	}

	var info unix.Stat_t
	if err = unix.Stat(path, &info); err != nil {
		return 0, 0, err
	}

	return start, info.Uid, nil
}

func WindowChild(parent int, command []string) error {
	if os.Getppid() != parent {
		return errors.New("backup buffer parent exited")
	}

	if err := unix.Kill(os.Getpid(), unix.SIGSTOP); err != nil {
		return err
	}

	return syscall.Exec(command[0], command, os.Environ())
}

func RunBuffer(runtimeDir, window string, command []string) (int, error) {
	lock, err := os.OpenFile(filepath.Join(runtimeDir, "lock"), os.O_RDWR, 0)
	if err != nil {
		return 1, err
	}
	defer lock.Close()

	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return 1, err
	}

	self, err := os.Executable()
	if err != nil {
		return 1, err
	}

	args := append([]string{"child", strconv.Itoa(os.Getpid())}, command...)
	child := exec.Command(self, args...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	// Linux associates Pdeathsig with the creating thread, so keep that thread alive.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err = child.Start(); err != nil {
		return 1, err
	}

	pid := child.Process.Pid
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		_ = child.Process.Kill()
		_ = child.Wait()
		return 1, err
	}
	defer unix.Close(pidfd)

	gate := ""
	waited := false
	defer func() {
		_ = unix.Flock(int(lock.Fd()), unix.LOCK_EX)
		_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
		if gate != "" {
			_ = os.Remove(gate)
		}

		if !waited {
			_ = child.Wait()
		}
	}()

	var status syscall.WaitStatus
	for {
		_, err = syscall.Wait4(pid, &status, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}

		break
	}

	if err != nil {
		return 1, err
	}

	if !status.Stopped() {
		waited = true
		_ = child.Process.Release()
		return waitStatus(status), nil
	}

	start, _, err := ProcessIdentity(pid)
	if err != nil {
		return 1, err
	}

	registration, err := os.CreateTemp(filepath.Join(runtimeDir, "gates"), fmt.Sprintf("%d-", pid))
	if err != nil {
		return 1, err
	}

	gate = registration.Name()
	_, err = fmt.Fprintf(registration, "%d %d\n", pid, start)
	closeErr := registration.Close()
	if err != nil {
		return 1, err
	}
	if closeErr != nil {
		return 1, closeErr
	}

	opened, err := windowOpen(window)
	if err != nil {
		return 1, err
	}

	if opened {
		if err = unix.PidfdSendSignal(pidfd, unix.SIGCONT, nil, 0); err != nil {
			return 1, err
		}
	}

	if err = unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
		return 1, err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, unix.SIGHUP, unix.SIGTERM)
	defer signal.Stop(signals)

	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	select {
	case err = <-done:
		waited = true
		return commandStatus(err)
	case signum := <-signals:
		_ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0)
		<-done
		waited = true
		return 128 + int(signum.(syscall.Signal)), nil
	}
}

func SignalBuffer(gate string, requested unix.Signal) error {
	fd, err := unix.Open(gate, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err == unix.ELOOP {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		unix.Close(fd)
		return err
	}

	if info.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return nil
	}

	data := make([]byte, 128)
	count, err := unix.Read(fd, data)
	unix.Close(fd)
	if err != nil {
		return err
	}

	removeStale := func() error {
		err := os.Remove(gate)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return err
	}

	if info.Mode&0022 != 0 {
		return removeStale()
	}

	fields := strings.Fields(string(data[:count]))
	if len(fields) != 2 {
		return removeStale()
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return removeStale()
	}

	start, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || start == 0 {
		return removeStale()
	}

	// Pin the process before reading /proc, so a reused PID cannot receive a signal.
	pidfd, err := unix.PidfdOpen(pid, 0)
	if err == unix.ESRCH || err == unix.EINVAL {
		return removeStale()
	}
	if err != nil {
		return err
	}
	defer unix.Close(pidfd)

	actual, uid, err := ProcessIdentity(pid)
	if errors.Is(err, os.ErrNotExist) || err == nil && (actual != start || uid != info.Uid) {
		return removeStale()
	}
	if err != nil {
		return err
	}

	err = unix.PidfdSendSignal(pidfd, requested, nil, 0)
	if err == unix.ESRCH {
		return removeStale()
	}

	return err
}

func ControlWindow(runtimeDir, window string) error {
	lock, err := os.OpenFile(filepath.Join(runtimeDir, "lock"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer lock.Close()

	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}

	opened, err := windowOpen(window)
	if err != nil {
		return err
	}

	requested := unix.SIGSTOP
	if opened {
		requested = unix.SIGCONT
	}

	gates, err := os.ReadDir(filepath.Join(runtimeDir, "gates"))
	if err != nil {
		return err
	}

	for _, gate := range gates {
		if err = SignalBuffer(filepath.Join(runtimeDir, "gates", gate.Name()), requested); err != nil {
			return err
		}
	}

	return nil
}
