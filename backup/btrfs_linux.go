//go:build linux && cgo

package backup

/*
#cgo LDFLAGS: -lbtrfsutil
#include <btrfsutil.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"time"
	"unsafe"
)

func btrfsError(path string, code C.enum_btrfs_util_error) error {
	if code == C.BTRFS_UTIL_OK {
		return nil
	}

	return fmt.Errorf("%s: %s", path, C.GoString(C.btrfs_util_strerror(code)))
}

func (Btrfs) IsSubvolume(path string) (bool, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))

	code := C.btrfs_util_is_subvolume(p)
	if code == C.BTRFS_UTIL_ERROR_NOT_BTRFS || code == C.BTRFS_UTIL_ERROR_NOT_SUBVOLUME {
		return false, nil
	}

	return code == C.BTRFS_UTIL_OK, btrfsError(path, code)
}

func (Btrfs) SubvolumeInfo(path string) (SubvolumeInfo, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))

	var raw C.struct_btrfs_util_subvolume_info
	if err := btrfsError(path, C.btrfs_util_subvolume_info(p, 0, &raw)); err != nil {
		return SubvolumeInfo{}, err
	}

	info := SubvolumeInfo{
		ID:         uint64(raw.id),
		ReceivedAt: time.Unix(int64(raw.rtime.tv_sec), int64(raw.rtime.tv_nsec)),
	}
	for i := range info.UUID {
		info.UUID[i] = byte(raw.uuid[i])
		info.ReceivedUUID[i] = byte(raw.received_uuid[i])
	}

	return info, nil
}

func (Btrfs) ReadOnly(path string) (bool, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))

	var readonly C.bool
	err := btrfsError(path, C.btrfs_util_get_subvolume_read_only(p, &readonly))
	return bool(readonly), err
}

func (Btrfs) HasDescendants(path string) (bool, error) {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))

	var iterator *C.struct_btrfs_util_subvolume_iterator
	if err := btrfsError(path, C.btrfs_util_create_subvolume_iterator(p, 0, 0, &iterator)); err != nil {
		return false, err
	}
	defer C.btrfs_util_destroy_subvolume_iterator(iterator)

	var id C.uint64_t
	code := C.btrfs_util_subvolume_iterator_next(iterator, nil, &id)
	if code == C.BTRFS_UTIL_ERROR_STOP_ITERATION {
		return false, nil
	}

	return code == C.BTRFS_UTIL_OK, btrfsError(path, code)
}

func (Btrfs) CreateSnapshot(source, target string) error {
	s, t := C.CString(source), C.CString(target)
	defer C.free(unsafe.Pointer(s))
	defer C.free(unsafe.Pointer(t))

	return btrfsError(target, C.btrfs_util_create_snapshot(s, t, C.BTRFS_UTIL_CREATE_SNAPSHOT_READ_ONLY, nil, nil))
}

func (Btrfs) DeleteSubvolume(path string) error {
	p := C.CString(path)
	defer C.free(unsafe.Pointer(p))

	// Do not pass the recursive flag: nested subvolumes must prevent deletion.
	return btrfsError(path, C.btrfs_util_delete_subvolume(p, 0))
}
