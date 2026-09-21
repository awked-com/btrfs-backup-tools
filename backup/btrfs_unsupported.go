//go:build !linux || !cgo

package backup

import "errors"

var errBtrfsUnavailable = errors.New("Btrfs operations require Linux and a build with libbtrfsutil")

func (Btrfs) IsSubvolume(string) (bool, error) { return false, errBtrfsUnavailable }

func (Btrfs) SubvolumeInfo(string) (SubvolumeInfo, error) {
	return SubvolumeInfo{}, errBtrfsUnavailable
}

func (Btrfs) ReadOnly(string) (bool, error) { return false, errBtrfsUnavailable }

func (Btrfs) HasDescendants(string) (bool, error) { return false, errBtrfsUnavailable }

func (Btrfs) CreateSnapshot(string, string) error { return errBtrfsUnavailable }

func (Btrfs) DeleteSubvolume(string) error { return errBtrfsUnavailable }
