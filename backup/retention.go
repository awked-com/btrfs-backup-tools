package backup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Backup struct {
	Path         string
	SnapshotTime time.Time
	ReceivedAt   time.Time
	SubvolumeID  uint64
	UUID         [16]byte
}

var snapshotSuffix = regexp.MustCompile(`^\.(\d{8}(?:T\d{4}|T\d{6}[+-]\d{4})?)(?:_\d+)?$`)

func SnapshotTime(name, basename string) (time.Time, bool) {
	if !strings.HasPrefix(name, basename) {
		return time.Time{}, false
	}

	match := snapshotSuffix.FindStringSubmatch(strings.TrimPrefix(name, basename))
	if match == nil {
		return time.Time{}, false
	}

	if len(match[1]) == 20 {
		hours, _ := strconv.Atoi(match[1][16:18])
		minutes, _ := strconv.Atoi(match[1][18:20])
		if hours >= 24 || minutes >= 60 {
			return time.Time{}, false
		}
	}

	layout := map[int]string{
		8:  "20060102",
		13: "20060102T1504",
		20: "20060102T150405-0700",
	}[len(match[1])]
	parsed, err := time.ParseInLocation(layout, match[1], time.Local)
	if err != nil || parsed.Year() < 1970 {
		return time.Time{}, false
	}

	return parsed.In(time.Local), true
}

func civilDay(t time.Time) int64 {
	y, m, d := t.In(time.Local).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

func RetainedBackups(backups []Backup, now time.Time, policy Policy) map[string]bool {
	keep := map[string]bool{}
	if len(backups) == 0 {
		return keep
	}

	ordered := append([]Backup(nil), backups...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].ReceivedAt.Equal(ordered[j].ReceivedAt) {
			return ordered[i].SubvolumeID < ordered[j].SubvolumeID
		}

		return ordered[i].ReceivedAt.Before(ordered[j].ReceivedAt)
	})
	keep[ordered[len(ordered)-1].Path] = true
	buckets := map[string]string{}
	today := civilDay(now)
	for _, backup := range ordered {
		date := backup.SnapshotTime.In(time.Local)
		day := civilDay(date)
		week := day - int64(date.Weekday())
		weekDate := time.Unix(week*86400, 0).UTC()
		// btrbk monthly buckets start on the first Sunday.
		month := int64(weekDate.Year()*12 + int(weekDate.Month()))
		if today-civilDay(backup.ReceivedAt) <= int64(policy.MinimumDays) || backup.SnapshotTime.After(now) {
			keep[backup.Path] = true
		}

		ages := []int64{today - day, (today - week) / 7, int64(now.Year()*12+int(now.Month())) - month}
		values := []int64{day, week, month}
		limits := []int{policy.Days, policy.Weeks, policy.Months}
		for tier, age := range ages {
			if age >= 0 && age <= int64(limits[tier]) {
				key := fmt.Sprintf("%d:%d", tier, values[tier])
				if _, ok := buckets[key]; !ok {
					buckets[key] = backup.Path
				}
			}
		}
	}

	for _, path := range buckets {
		keep[path] = true
	}

	return keep
}

func complete(info SubvolumeInfo, readonly bool) bool {
	return readonly && info.ReceivedUUID != [16]byte{} && info.ReceivedAt.After(time.Unix(0, 0))
}

func ReadBackups(directory string, btrfs Backend) ([]Backup, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}

	backups := []Backup{}
	for _, entry := range entries {
		created, valid := SnapshotTime(entry.Name(), filepath.Base(directory))
		if !valid || !entry.IsDir() {
			continue
		}

		path := filepath.Join(directory, entry.Name())
		valid, err = btrfs.IsSubvolume(path)
		if err != nil {
			return nil, err
		}
		if !valid {
			continue
		}

		info, err := btrfs.SubvolumeInfo(path)
		if err != nil {
			return nil, err
		}

		readonly, err := btrfs.ReadOnly(path)
		if err != nil {
			return nil, err
		}

		if complete(info, readonly) {
			backups = append(backups, Backup{path, created, info.ReceivedAt, info.ID, info.UUID})
		}
	}

	return backups, nil
}

func PruneDirectory(directory string, btrfs Backend, now time.Time, policy Policy, apply bool, output io.Writer) error {
	backups, err := ReadBackups(directory, btrfs)
	if err != nil {
		return err
	}

	keep := RetainedBackups(backups, now, policy)
	for _, backup := range backups {
		if keep[backup.Path] {
			continue
		}

		current, err := btrfs.SubvolumeInfo(backup.Path)
		if err != nil {
			return err
		}

		readonly, err := btrfs.ReadOnly(backup.Path)
		if err != nil {
			return err
		}
		if current.UUID != backup.UUID || current.ID != backup.SubvolumeID || !current.ReceivedAt.Equal(backup.ReceivedAt) || !readonly {
			return fmt.Errorf("backup changed during retention: %s", backup.Path)
		}

		action := "Would delete"
		if apply {
			action = "Deleting"
		}

		fmt.Fprintf(output, "%s %s\n", action, backup.Path)
		if apply {
			if err := btrfs.DeleteSubvolume(backup.Path); err != nil {
				return err
			}
		}
	}

	return nil
}

func RunRetention(config Config, apply bool, output io.Writer) error {
	mounted, err := IsMount(config.Root)
	if err != nil {
		return err
	}
	if !mounted {
		return fmt.Errorf("backup target is not mounted: %s", config.Root)
	}

	now := time.Now()
	for _, directory := range config.Directories {
		if !within(directory, config.Root) {
			return fmt.Errorf("invalid backup directory: %s", directory)
		}

		if err := canonicalDirectory(directory); err != nil {
			return err
		}

		lock, err := ReceiverLock(config.DirectoryLocks[directory], true)
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			fmt.Fprintf(output, "Skipping busy replica directory: %s\n", directory)
			continue
		}

		if err != nil {
			return err
		}

		err = PruneDirectory(directory, Btrfs{}, now, config.Retention, apply, output)
		lock.Close()
		if err != nil {
			return err
		}
	}

	return nil
}
