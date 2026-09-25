package mounter_test

import (
	"math"
	"sync"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/driver/node/mounter"
	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestUIDAllocator(t *testing.T) {
	t.Run("Hands out unique UIDs from within the range", func(t *testing.T) {
		a := mounter.NewUIDAllocator()
		seen := make(map[uint32]struct{})

		for range 1000 {
			uid, err := a.Allocate()
			assert.NoError(t, err)

			if uid < mounter.UIDRangeStart || uid > mounter.UIDRangeEnd {
				t.Fatalf("UID %d outside [%d, %d]", uid, mounter.UIDRangeStart, mounter.UIDRangeEnd)
			}
			if _, dup := seen[uid]; dup {
				t.Fatalf("UID %d handed out twice", uid)
			}
			seen[uid] = struct{}{}
		}
	})

	t.Run("Reuses a released UID rather than failing when the range is full", func(t *testing.T) {
		a := mounter.NewUIDAllocator()
		allocateAll(t, a)

		// With everything taken, the next allocation must fail and the returned UID must not be zero.
		exhausted, err := a.Allocate()
		assert.ErrorIs(t, err, mounter.ErrUIDRangeExhausted)
		assert.Equals(t, uint32(math.MaxUint32), exhausted)

		// Freeing one makes exactly that UID available again.
		a.Release(mounter.UIDRangeStart + 42)
		uid, err := a.Allocate()
		assert.NoError(t, err)
		assert.Equals(t, uint32(mounter.UIDRangeStart+42), uid)

		_, err = a.Allocate()
		assert.ErrorIs(t, err, mounter.ErrUIDRangeExhausted)
	})

	t.Run("Wraps past the end of the range to find a free UID", func(t *testing.T) {
		a := mounter.NewUIDAllocator()

		// Fill the range, then free one near the bottom. The cursor is now at the top, so finding it
		// requires wrapping.
		allocateAll(t, a)
		a.Release(mounter.UIDRangeStart + 1)

		uid, err := a.Allocate()
		assert.NoError(t, err)
		assert.Equals(t, uint32(mounter.UIDRangeStart+1), uid)
	})

	t.Run("Does not reallocate a reserved UID", func(t *testing.T) {
		a := mounter.NewUIDAllocator()
		seeded := uint32(mounter.UIDRangeStart + 5)
		assert.NoError(t, a.Reserve(seeded))

		assert.Equals(t, true, a.InUse(seeded))

		// Enough allocations to scan well past the reserved value.
		for range 10 {
			uid, err := a.Allocate()
			assert.NoError(t, err)
			if uid == seeded {
				t.Fatalf("reallocated reserved UID %d", seeded)
			}
		}
	})

	t.Run("Rejects reservations outside the range", func(t *testing.T) {
		a := mounter.NewUIDAllocator()

		// These arrive from a meta file on disk, so a zero or out-of-range value is possible. The
		// caller is told rather than left to assume the reservation took effect.
		for _, uid := range []uint32{0, mounter.UIDRangeStart - 1, mounter.UIDRangeEnd + 1} {
			assert.ErrorIs(t, a.Reserve(uid), mounter.ErrUIDOutOfRange)
			assert.Equals(t, false, a.InUse(uid))
		}

		uid, err := a.Allocate()
		assert.NoError(t, err)
		assert.Equals(t, uint32(mounter.UIDRangeStart), uid)
	})

	t.Run("Concurrent allocations never collide", func(t *testing.T) {
		a := mounter.NewUIDAllocator()

		const goroutines = 50
		const perGoroutine = 20

		var wg sync.WaitGroup
		results := make(chan uint32, goroutines*perGoroutine)

		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perGoroutine {
					uid, err := a.Allocate()
					if err != nil {
						t.Errorf("unexpected allocation failure: %v", err)
						return
					}
					results <- uid
				}
			}()
		}

		wg.Wait()
		close(results)

		seen := make(map[uint32]struct{})
		for uid := range results {
			if _, dup := seen[uid]; dup {
				t.Fatalf("UID %d handed out twice under concurrency", uid)
			}
			seen[uid] = struct{}{}
		}
		assert.Equals(t, goroutines*perGoroutine, len(seen))
	})
}

// allocateAll claims every UID in the range.
func allocateAll(t *testing.T, a *mounter.UIDAllocator) {
	t.Helper()
	for range mounter.UIDRangeEnd - mounter.UIDRangeStart + 1 {
		if _, err := a.Allocate(); err != nil {
			t.Fatalf("failed to fill the range: %v", err)
		}
	}
}
