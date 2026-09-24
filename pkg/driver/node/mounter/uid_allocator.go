package mounter

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

const (
	// UIDRangeStart and UIDRangeEnd bound the UIDs handed to Mountpoint processes, inclusive.
	//
	// The range is based on the assumption that the host's files and processes use UIDs/GIDs values
	// below this range, which is standard for most Linux distributions. This approach avoids overlap
	// with the UIDs/GIDs of the host. The range's capacity is far larger than the
	// `maxVolumesPerNode` default of 4, so exhaustion is not a practical concern.
	UIDRangeStart = 65536
	UIDRangeEnd   = 131071
	UIDRangeSize  = UIDRangeEnd - UIDRangeStart + 1
)

var ErrUIDRangeExhausted = errors.New("no free UID in range")
var ErrUIDOutOfRange = errors.New("UID outside the allocatable range")

// UIDAllocator hands out and releases unique UIDs from [UIDRangeStart, UIDRangeEnd].
//
// [DaemonsetMounter] uses it to give every Mountpoint process its own UID and persists the allocated
// IDs in [MountMeta], from which it also repopulates the allocator via [UIDAllocator.Reserve] on startup.
type UIDAllocator struct {
	mu        sync.Mutex
	allocated map[uint32]struct{}

	// cursor is the UID the next Allocate starts scanning from.
	cursor uint32
}

// NewUIDAllocator creates an allocator with nothing allocated.
func NewUIDAllocator() *UIDAllocator {
	return &UIDAllocator{
		allocated: make(map[uint32]struct{}),
		cursor:    UIDRangeStart,
	}
}

// Allocate claims and returns a free UID, or [ErrUIDRangeExhausted] if there is none.
func (a *UIDAllocator) Allocate() (uint32, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Scan the range once from the cursor, wrapping at the end.
	// A full unsuccessful pass returns exhaustion error.
	for range UIDRangeSize {
		candidate := a.cursor

		a.cursor += 1
		if a.cursor > UIDRangeEnd {
			a.cursor = UIDRangeStart
		}

		if _, taken := a.allocated[candidate]; !taken {
			a.allocated[candidate] = struct{}{}
			return candidate, nil
		}
	}

	return math.MaxUint32, ErrUIDRangeExhausted
}

// Release makes a UID available to [UIDAllocator.Allocate] again.
func (a *UIDAllocator) Release(uid uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.allocated, uid)
}

// Reserve makes a UID unavailable to [UIDAllocator.Allocate], returning [ErrUIDOutOfRange] for a UID
// outside the range.
//
// [DaemonsetMounter] calls this when repopulating the [UIDAllocator] on startup from the [MountMeta].
func (a *UIDAllocator) Reserve(uid uint32) error {
	if uid < UIDRangeStart || uid > UIDRangeEnd {
		return fmt.Errorf("cannot reserve %d: %w", uid, ErrUIDOutOfRange)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.allocated[uid] = struct{}{}
	return nil
}

// InUse reports whether `uid` is currently allocated. Exported for tests and diagnostics.
func (a *UIDAllocator) InUse(uid uint32) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	_, taken := a.allocated[uid]
	return taken
}
