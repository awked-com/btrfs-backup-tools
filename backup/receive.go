package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"golang.org/x/sys/unix"
)

func CheckedReceiveTarget(target string, config Config) error {
	if !slices.Contains(config.Directories, target) || !filepath.IsAbs(target) || filepath.Clean(target) != target || !within(target, config.Root) {
		return errors.New("receive destination is not an assigned replica directory")
	}

	for path := target; ; path = filepath.Dir(path) {
		var info unix.Stat_t
		if err := unix.Lstat(path, &info); err != nil {
			return err
		}

		if info.Mode&unix.S_IFMT != unix.S_IFDIR || info.Uid != 0 || info.Mode&0022 != 0 {
			return errors.New("receive path must contain only root-owned, protected directories")
		}
		if path == filepath.Dir(path) {
			break
		}
	}

	mounted, err := IsMount(config.Root)
	if err != nil {
		return err
	}
	if !mounted {
		return errors.New("backup target is not mounted")
	}

	return nil
}

func RequireDeviceFilter() error {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}

	group := ""
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "0::/") {
			group = line[3:]
			break
		}
	}

	if group == "" {
		return errors.New("receive requires the unified cgroup hierarchy")
	}

	cgroup, err := os.Open(filepath.Join("/sys/fs/cgroup", strings.TrimLeft(group, "/")))
	if err != nil {
		return err
	}
	defer cgroup.Close()
	attached, err := link.QueryPrograms(link.QueryOptions{Target: int(cgroup.Fd()), Attach: ebpf.AttachCGroupDevice})
	if err != nil {
		return err
	}
	for _, program := range attached.Programs {
		handle, err := ebpf.NewProgramFromID(program.ID)
		if err != nil {
			return err
		}
		info, err := handle.Info()
		handle.Close()
		if err != nil {
			return err
		}
		if info.Name == "sd_devices" {
			return nil
		}
	}

	return errors.New("receive scope has no enforced systemd device filter")
}

type ReceiveDeadline struct {
	limits            Limits
	started, previous time.Time
	active, idle      time.Duration
}

type InterruptedError struct{ Signal syscall.Signal }

func (e *InterruptedError) Error() string {
	return fmt.Sprintf("backup receive interrupted by %s", e.Signal)
}

func NewReceiveDeadline(limits Limits, now time.Time) *ReceiveDeadline {
	return &ReceiveDeadline{
		limits:   limits,
		started:  now,
		previous: now,
	}
}

func (d *ReceiveDeadline) Update(now time.Time, opened, progress bool) error {
	elapsed := now.Sub(d.previous)
	d.previous = now
	if opened {
		d.active += elapsed
		d.idle += elapsed
	}

	if progress {
		d.idle = 0
	}

	if now.Sub(d.started).Seconds() >= d.limits.WallSeconds {
		return errors.New("backup receive exceeded its wall-clock limit")
	}
	if d.active.Seconds() >= d.limits.ActiveSeconds {
		return errors.New("backup receive exceeded its active transfer limit")
	}
	if d.idle.Seconds() >= d.limits.IdleSeconds {
		return errors.New("backup receive made no progress before its idle limit")
	}

	return nil
}

func windowOpen(command string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := exec.CommandContext(ctx, command).Run()
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return false, nil
	}

	return err == nil, err
}

func SuperviseReceive(command []string, config Config, inputFD int) error {
	if config.Limits.WallSeconds <= 0 || config.Limits.ActiveSeconds <= 0 || config.Limits.IdleSeconds <= 0 {
		return errors.New("positive wall, active, and idle receive limits are required")
	}

	deadline := NewReceiveDeadline(config.Limits, time.Now())
	flags, err := unix.FcntlInt(uintptr(inputFD), unix.F_GETFL, 0)
	if err != nil {
		return err
	}

	if err = unix.SetNonblock(inputFD, true); err != nil {
		return err
	}
	defer unix.FcntlInt(uintptr(inputFD), unix.F_SETFL, flags)

	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer writer.Close()

	process := exec.Command(command[0], command[1:]...)
	process.Stdin = reader
	process.Stdout = os.Stdout
	process.Stderr = os.Stderr
	process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = process.Start()
	reader.Close()
	if err != nil {
		return err
	}

	finished := false
	defer func() {
		if !finished {
			_ = unix.Kill(-process.Process.Pid, unix.SIGKILL)
			_ = process.Wait()
		}
	}()

	writeFD := int(writer.Fd())
	if err = unix.SetNonblock(writeFD, true); err != nil {
		return err
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, unix.SIGHUP, unix.SIGTERM)
	defer signal.Stop(signals)

	var pending []byte
	buffer := make([]byte, 65536)
	opened := true
	var nextWindowCheck time.Time
	eof := false
	for {
		select {
		case sig := <-signals:
			return &InterruptedError{Signal: sig.(syscall.Signal)}
		default:
		}

		// Reap here, never in a concurrent waiter: cleanup must retain ownership of
		// the child's numeric process-group ID until it has finished signaling it.
		var status syscall.WaitStatus
		pid, waitErr := syscall.Wait4(process.Process.Pid, &status, syscall.WNOHANG, nil)
		if waitErr == syscall.EINTR {
			continue
		}

		if waitErr != nil {
			if waitErr == syscall.ECHILD {
				finished = true
				_ = process.Process.Release()
			}

			return waitErr
		}

		if pid != 0 {
			finished = true
			_ = process.Process.Release()
			if status.ExitStatus() == 0 {
				return nil
			}

			return fmt.Errorf("backup receive failed: %s", statusDescription(status))
		}

		now := time.Now()
		if config.TransferWindow != "" && !now.Before(nextWindowCheck) {
			opened, err = windowOpen(config.TransferWindow)
			if err != nil {
				return err
			}

			nextWindowCheck = now.Add(5 * time.Second)
		}

		if err = deadline.Update(now, opened, false); err != nil {
			return err
		}

		var poll []unix.PollFd
		if len(pending) > 0 {
			poll = []unix.PollFd{{Fd: int32(writeFD), Events: unix.POLLOUT}}
		} else if !eof {
			poll = []unix.PollFd{{Fd: int32(inputFD), Events: unix.POLLIN}}
		}

		_, err = unix.Poll(poll, 200)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if len(poll) == 0 || poll[0].Revents == 0 {
			continue
		}

		if len(pending) > 0 {
			written, err := unix.Write(writeFD, pending)
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if err != nil {
				return err
			}

			pending = pending[written:]
			if err = deadline.Update(time.Now(), opened, written > 0); err != nil {
				return err
			}
		} else {
			count, err := unix.Read(inputFD, buffer)
			if err == unix.EAGAIN || err == unix.EINTR {
				continue
			}
			if err != nil {
				return err
			}

			if count == 0 {
				eof = true
				writer.Close()
			} else {
				pending = buffer[:count]
			}
		}
	}
}

func statusDescription(status syscall.WaitStatus) string {
	if status.Signaled() {
		return fmt.Sprintf("signal %s", status.Signal())
	}

	return fmt.Sprintf("exit status %d", status.ExitStatus())
}

func PublishReplica(stage, target string, btrfs Backend) error {
	children, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	if len(children) != 1 {
		return errors.New("receive must produce exactly one replica")
	}

	child := children[0]
	if _, valid := SnapshotTime(child.Name(), filepath.Base(target)); !valid {
		return errors.New("received replica has an invalid name")
	}

	replica := filepath.Join(stage, child.Name())
	if !child.IsDir() {
		return errors.New("received replica is not a subvolume")
	}

	valid, err := btrfs.IsSubvolume(replica)
	if err != nil {
		return err
	}
	if !valid {
		return errors.New("received replica is not a subvolume")
	}

	info, err := btrfs.SubvolumeInfo(replica)
	if err != nil {
		return err
	}

	readonly, err := btrfs.ReadOnly(replica)
	if err != nil {
		return err
	}
	if !complete(info, readonly) {
		return errors.New("received replica is not complete and read-only")
	}

	nested, err := btrfs.HasDescendants(replica)
	if err != nil {
		return err
	}
	if nested {
		return errors.New("received replica contains nested subvolumes")
	}

	// Read-only subvolumes cannot be renamed; snapshots preserve received UUIDs.
	if err = btrfs.CreateSnapshot(replica, filepath.Join(target, child.Name())); err != nil {
		return err
	}

	return btrfs.DeleteSubvolume(replica)
}

func ReceiveReplica(target string, config Config, btrfs Backend) error {
	staging := config.Staging
	if filepath.Dir(staging) != config.Root || filepath.Clean(staging) != staging {
		return errors.New("staging directory must be directly beneath the backup mount")
	}

	var stagingInfo, targetInfo unix.Stat_t
	if err := unix.Lstat(staging, &stagingInfo); err != nil {
		return err
	}

	if stagingInfo.Mode&unix.S_IFMT != unix.S_IFDIR || stagingInfo.Uid != 0 || stagingInfo.Mode&0077 != 0 {
		return errors.New("staging directory must be private and root-owned")
	}

	if err := unix.Stat(target, &targetInfo); err != nil {
		return err
	}

	if stagingInfo.Dev != targetInfo.Dev {
		return errors.New("staging and replica directories must share a filesystem")
	}

	return receiveStaged(target, config, btrfs)
}

func receiveStaged(target string, config Config, btrfs Backend) (result error) {
	stage, err := os.MkdirTemp(config.Staging, "receive-")
	if err != nil {
		return err
	}

	// Keep the lease outside the sender-controlled chroot and retain failed streams.
	leasePath := stage + ".lock"
	fd, err := unix.Open(leasePath, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			fmt.Fprintf(os.Stderr, "Receive staging retained for inspection: %s\n", stage)
		}
	}()

	if err = SuperviseReceive([]string{config.Btrfs, "receive", "--chroot", stage}, config, int(os.Stdin.Fd())); err != nil {
		return err
	}

	if err = PublishReplica(stage, target, btrfs); err != nil {
		return err
	}

	if err = os.Remove(stage); err != nil {
		return err
	}

	return os.Remove(leasePath)
}

func RunConfinedReceive(configPath, target string, config Config, confined bool) error {
	if err := CheckedReceiveTarget(target, config); err != nil {
		return err
	}

	if confined {
		if err := RequireDeviceFilter(); err != nil {
			return err
		}

		sender, err := ReceiverLock(config.ReceiveLock, true)
		if err != nil {
			return err
		}
		defer sender.Close()

		directory, err := ReceiverLock(config.DirectoryLocks[target], false)
		if err != nil {
			return err
		}
		defer directory.Close()

		return ReceiveReplica(target, config, Btrfs{})
	}

	if config.Limits.MemoryBytes == 0 || config.Limits.Tasks <= 0 || config.Limits.WallSeconds <= 0 {
		return errors.New("positive memory, tasks, and wall receive limits are required")
	}

	self, err := os.Executable()
	if err != nil {
		return err
	}

	return syscall.Exec(config.SystemdRun, receiveScopeArgs(self, configPath, target, config), os.Environ())
}

func receiveScopeArgs(self, configPath, target string, config Config) []string {
	return []string{
		config.SystemdRun,
		"--scope",
		"--quiet",
		"--collect",
		"--no-ask-password",
		"--expand-environment=no",
		"--property=DevicePolicy=closed",
		fmt.Sprintf("--property=MemoryMax=%d", config.Limits.MemoryBytes),
		"--property=MemorySwapMax=0",
		"--property=OOMPolicy=kill",
		fmt.Sprintf("--property=TasksMax=%d", config.Limits.Tasks),
		// Systemd durations accept decimal seconds, not scientific notation.
		"--property=RuntimeMaxSec=" + strconv.FormatFloat(config.Limits.WallSeconds, 'f', -1, 64),
		"--", self,
		"--config", configPath,
		"--confined",
		"--", target,
	}
}
