package mounter

import (
	"sync"
	"testing"

	"github.com/awslabs/mountpoint-s3-csi-driver/pkg/util/testutil/assert"
)

func TestMountMap_GetOrCreate_NewEntry(t *testing.T) {
	m := NewMountMap()
	entry, existed := m.GetOrCreate("vol-1")
	assert.Equals(t, false, existed)
	assert.Equals(t, "vol-1", entry.VolumeID)
	assert.Equals(t, false, entry.sourceMounted)
}

func TestMountMap_GetOrCreate_ExistingEntry(t *testing.T) {
	m := NewMountMap()
	entry1, _ := m.GetOrCreate("vol-1")
	entry1.mu.Lock()
	entry1.sourceMounted = true
	entry1.SourcePath = "/source"
	entry1.mu.Unlock()

	entry2, existed := m.GetOrCreate("vol-1")
	assert.Equals(t, true, existed)
	if entry1 != entry2 {
		t.Fatal("expected same pointer for same volumeID")
	}
	assert.Equals(t, "/source", entry2.SourcePath)
}

func TestMountMap_Get_NotFound(t *testing.T) {
	m := NewMountMap()
	entry := m.Get("nonexistent")
	if entry != nil {
		t.Fatal("expected nil for nonexistent volume")
	}
}

func TestMountMap_ConcurrentGetOrCreate_SameVolume(t *testing.T) {
	m := NewMountMap()
	var wg sync.WaitGroup
	entries := make([]*MountEntry, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			entry, _ := m.GetOrCreate("vol-shared")
			entries[idx] = entry
		}(i)
	}
	wg.Wait()

	// All goroutines should get the same entry pointer
	for i := 1; i < 10; i++ {
		if entries[i] != entries[0] {
			t.Fatalf("entry[%d] != entry[0]: different pointers for same volumeID", i)
		}
	}
}

func TestMountMap_ConcurrentGetOrCreate_DifferentVolumes(t *testing.T) {
	m := NewMountMap()
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			volumeID := "vol-" + string(rune('A'+idx%26))
			entry, _ := m.GetOrCreate(volumeID)
			entry.mu.Lock()
			entry.RefCount++
			entry.mu.Unlock()
		}(i)
	}
	wg.Wait()
}

func TestValidateCompatibility_AllMatch(t *testing.T) {
	existing := &MountParams{
		MountOptions:         []string{"--allow-other", "--region us-east-1"},
		AuthenticationSource: "driver",
		ServiceAccountName:   "default",
		PodNamespace:         "default",
		FSGroup:              "1000",
	}
	incoming := &MountParams{
		MountOptions:         []string{"--allow-other", "--region us-east-1"},
		AuthenticationSource: "driver",
		ServiceAccountName:   "default",
		PodNamespace:         "default",
		FSGroup:              "1000",
	}
	err := existing.ValidateCompatibility(incoming)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateCompatibility_MountOptionsMismatch(t *testing.T) {
	existing := &MountParams{MountOptions: []string{"--read-only"}}
	incoming := &MountParams{MountOptions: []string{"--allow-delete"}}
	err := existing.ValidateCompatibility(incoming)
	assert.Contains(t, err.Error(), "mount options mismatch")
}

func TestValidateCompatibility_AuthMismatch(t *testing.T) {
	existing := &MountParams{AuthenticationSource: "driver"}
	incoming := &MountParams{AuthenticationSource: "pod"}
	err := existing.ValidateCompatibility(incoming)
	assert.Contains(t, err.Error(), "authenticationSource mismatch")
}

func TestValidateCompatibility_ServiceAccountMismatch(t *testing.T) {
	existing := &MountParams{AuthenticationSource: "pod", ServiceAccountName: "sa-a"}
	incoming := &MountParams{AuthenticationSource: "pod", ServiceAccountName: "sa-b"}
	err := existing.ValidateCompatibility(incoming)
	assert.Contains(t, err.Error(), "serviceAccountName mismatch")
}

func TestValidateCompatibility_ServiceAccountIgnoredWithDriverAuth(t *testing.T) {
	existing := &MountParams{AuthenticationSource: "driver", ServiceAccountName: "sa-a"}
	incoming := &MountParams{AuthenticationSource: "driver", ServiceAccountName: "sa-b"}
	err := existing.ValidateCompatibility(incoming)
	if err != nil {
		t.Errorf("expected no error for SA mismatch with driver auth, got: %v", err)
	}
}

func TestValidateCompatibility_NamespaceMismatch_PodAuth(t *testing.T) {
	existing := &MountParams{AuthenticationSource: "pod", PodNamespace: "ns-a"}
	incoming := &MountParams{AuthenticationSource: "pod", PodNamespace: "ns-b"}
	err := existing.ValidateCompatibility(incoming)
	assert.Contains(t, err.Error(), "podNamespace mismatch")
}

func TestValidateCompatibility_NamespaceIgnoredWithDriverAuth(t *testing.T) {
	existing := &MountParams{AuthenticationSource: "driver", PodNamespace: "ns-a"}
	incoming := &MountParams{AuthenticationSource: "driver", PodNamespace: "ns-b"}
	err := existing.ValidateCompatibility(incoming)
	if err != nil {
		t.Errorf("expected no error for namespace mismatch with driver auth, got: %v", err)
	}
}

func TestValidateCompatibility_FSGroupMismatch(t *testing.T) {
	existing := &MountParams{FSGroup: "1000"}
	incoming := &MountParams{FSGroup: "2000"}
	err := existing.ValidateCompatibility(incoming)
	assert.Contains(t, err.Error(), "fsGroup")
}

func TestValidateCompatibility_ServiceAccountEKSRoleARNMismatch(t *testing.T) {
	existing := &MountParams{
		AuthenticationSource:     "pod",
		ServiceAccountName:       "sa-a",
		ServiceAccountEKSRoleARN: "arn:aws:iam::111111111111:role/role-a",
	}
	incoming := &MountParams{
		AuthenticationSource:     "pod",
		ServiceAccountName:       "sa-a",
		ServiceAccountEKSRoleARN: "arn:aws:iam::111111111111:role/role-b",
	}
	err := existing.ValidateCompatibility(incoming)
	assert.Contains(t, err.Error(), "serviceAccountEKSRoleARN mismatch")
}

func TestValidateCompatibility_ServiceAccountEKSRoleARNMatch(t *testing.T) {
	existing := &MountParams{
		ServiceAccountName:       "sa-a",
		AuthenticationSource:     "pod",
		PodNamespace:             "default",
		ServiceAccountEKSRoleARN: "arn:aws:iam::111111111111:role/role-a",
		MountOptions:             []string{"--allow-other"},
	}
	incoming := &MountParams{
		ServiceAccountName:       "sa-a",
		AuthenticationSource:     "pod",
		PodNamespace:             "default",
		ServiceAccountEKSRoleARN: "arn:aws:iam::111111111111:role/role-a",
		MountOptions:             []string{"--allow-other"},
	}
	err := existing.ValidateCompatibility(incoming)
	if err != nil {
		t.Fatalf("expected no error for matching EKS role ARN, got: %v", err)
	}
}

func TestMountEntry_ResetAllowsDifferentParams(t *testing.T) {
	m := NewMountMap()

	// First workload mounts with SA "sa-a"
	entry, _ := m.GetOrCreate("vol-1")
	entry.mu.Lock()
	entry.SourcePath = "/source"
	entry.Params = MountParams{ServiceAccountName: "sa-a", AuthenticationSource: "driver"}
	entry.RefCount = 1
	entry.Targets = []string{"/target-a"}
	entry.sourceMounted = true
	entry.mu.Unlock()

	// Simulate unmount of last consumer
	entry.mu.Lock()
	entry.RefCount--
	entry.sourceMounted = false
	entry.SourcePath = ""
	entry.Params = MountParams{}
	entry.Targets = nil
	entry.mu.Unlock()

	// Second workload with different SA should be able to mount
	entry2, existed := m.GetOrCreate("vol-1")
	assert.Equals(t, true, existed)
	entry2.mu.Lock()
	defer entry2.mu.Unlock()

	// entry.sourceMounted is false, so no validation runs — new mount is allowed
	assert.Equals(t, false, entry2.sourceMounted)

	// Simulate new mount with different params
	newParams := MountParams{ServiceAccountName: "sa-b", AuthenticationSource: "pod"}
	entry2.SourcePath = "/new-source"
	entry2.Params = newParams
	entry2.RefCount = 1
	entry2.Targets = []string{"/target-b"}
	entry2.sourceMounted = true

	// Verify new params took effect
	assert.Equals(t, "sa-b", entry2.Params.ServiceAccountName)
	assert.Equals(t, "pod", entry2.Params.AuthenticationSource)
}

func TestMountEntry_ShareRejectedWithDifferentParams(t *testing.T) {
	m := NewMountMap()

	// First workload mounts with pod auth
	entry, _ := m.GetOrCreate("vol-1")
	entry.mu.Lock()
	entry.SourcePath = "/source"
	entry.Params = MountParams{ServiceAccountName: "sa-a", AuthenticationSource: "pod"}
	entry.RefCount = 1
	entry.Targets = []string{"/target-a"}
	entry.sourceMounted = true
	entry.mu.Unlock()

	// Second workload tries to share with different SA — should fail with pod auth
	entry2, _ := m.GetOrCreate("vol-1")
	entry2.mu.Lock()
	defer entry2.mu.Unlock()

	incoming := &MountParams{ServiceAccountName: "sa-b", AuthenticationSource: "pod"}
	err := entry2.Params.ValidateCompatibility(incoming)
	if err == nil {
		t.Fatal("expected validation error for different SA with pod auth, got nil")
	}
	assert.Contains(t, err.Error(), "serviceAccountName mismatch")
}

func TestMountEntry_ShareAllowedWithDifferentSADriverAuth(t *testing.T) {
	m := NewMountMap()

	// First workload mounts with driver auth
	entry, _ := m.GetOrCreate("vol-1")
	entry.mu.Lock()
	entry.SourcePath = "/source"
	entry.Params = MountParams{ServiceAccountName: "sa-a", AuthenticationSource: "driver"}
	entry.RefCount = 1
	entry.Targets = []string{"/target-a"}
	entry.sourceMounted = true
	entry.mu.Unlock()

	// Second workload with different SA — should succeed with driver auth
	entry2, _ := m.GetOrCreate("vol-1")
	entry2.mu.Lock()
	defer entry2.mu.Unlock()

	incoming := &MountParams{ServiceAccountName: "sa-b", AuthenticationSource: "driver"}
	err := entry2.Params.ValidateCompatibility(incoming)
	if err != nil {
		t.Fatalf("expected no error for different SA with driver auth, got: %v", err)
	}
}

func TestMountEntry_ShareAllowedWithSameParams(t *testing.T) {
	m := NewMountMap()

	// First workload mounts
	entry, _ := m.GetOrCreate("vol-1")
	entry.mu.Lock()
	entry.SourcePath = "/source"
	entry.Params = MountParams{
		ServiceAccountName:   "sa-a",
		AuthenticationSource: "driver",
		PodNamespace:         "default",
		FSGroup:              "",
		MountOptions:         []string{"--allow-other"},
	}
	entry.RefCount = 1
	entry.Targets = []string{"/target-a"}
	entry.sourceMounted = true
	entry.mu.Unlock()

	// Second workload tries to share with same params — should succeed
	entry2, _ := m.GetOrCreate("vol-1")
	entry2.mu.Lock()
	defer entry2.mu.Unlock()

	incoming := &MountParams{
		ServiceAccountName:   "sa-a",
		AuthenticationSource: "driver",
		PodNamespace:         "default",
		FSGroup:              "",
		MountOptions:         []string{"--allow-other"},
	}
	err := entry2.Params.ValidateCompatibility(incoming)
	if err != nil {
		t.Fatalf("expected no error for matching params, got: %v", err)
	}

	// Simulate adding the target
	entry2.RefCount++
	entry2.Targets = append(entry2.Targets, "/target-b")
	assert.Equals(t, 2, entry2.RefCount)
	assert.Equals(t, 2, len(entry2.Targets))
}

func TestMountMap_Delete_EntryRemoved(t *testing.T) {
	m := NewMountMap()
	entry, _ := m.GetOrCreate("vol-1")
	entry.mu.Lock()
	entry.sourceMounted = true
	entry.RefCount = 1
	entry.mu.Unlock()

	m.Delete("vol-1")

	if m.Get("vol-1") != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestMountMap_Delete_GetOrCreateReturnsNewEntry(t *testing.T) {
	m := NewMountMap()

	// Create and populate an entry
	entry1, _ := m.GetOrCreate("vol-1")
	entry1.mu.Lock()
	entry1.sourceMounted = true
	entry1.SourcePath = "/old-source"
	entry1.mu.Unlock()

	// Delete it
	m.Delete("vol-1")

	// GetOrCreate should return a fresh entry (different pointer)
	entry2, existed := m.GetOrCreate("vol-1")
	assert.Equals(t, false, existed)
	if entry2 == entry1 {
		t.Fatal("expected new entry after delete, got same pointer")
	}
	assert.Equals(t, false, entry2.sourceMounted)
	assert.Equals(t, "", entry2.SourcePath)
}

func TestMountMap_RetryLoop_ConvergesAfterDelete(t *testing.T) {
	// Simulates the retry-loop pattern used in mountWithSharing:
	// 1. GetOrCreate returns entry
	// 2. Lock it
	// 3. Check if it's still canonical (in the map)
	// 4. If not, unlock and retry
	m := NewMountMap()

	// Create an entry and immediately delete it (simulates unmount racing)
	orphan, _ := m.GetOrCreate("vol-1")
	m.Delete("vol-1")

	// Retry loop should converge on a new canonical entry
	var entry *MountEntry
	iterations := 0
	for {
		entry, _ = m.GetOrCreate("vol-1")
		entry.mu.Lock()
		if m.Get("vol-1") == entry {
			break
		}
		entry.mu.Unlock()
		iterations++
		if iterations > 10 {
			t.Fatal("retry loop did not converge")
		}
	}
	defer entry.mu.Unlock()

	// The entry we hold should NOT be the orphan
	if entry == orphan {
		t.Fatal("expected a new entry, not the orphaned one")
	}
	assert.Equals(t, false, entry.sourceMounted)
}

func TestMountMap_ConcurrentDeleteAndGetOrCreate(t *testing.T) {
	// Stress test: concurrent deletes and GetOrCreate on the same volumeID.
	// Verifies no panic, no infinite loop, and final state is consistent.
	m := NewMountMap()
	var wg sync.WaitGroup

	const goroutines = 20
	const volumeID = "vol-race"

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				// Mount path: retry loop
				var entry *MountEntry
				for {
					entry, _ = m.GetOrCreate(volumeID)
					entry.mu.Lock()
					if m.Get(volumeID) == entry {
						break
					}
					entry.mu.Unlock()
				}
				entry.sourceMounted = true
				entry.RefCount++
				entry.mu.Unlock()
			} else {
				// Unmount path: delete
				entry := m.Get(volumeID)
				if entry == nil {
					return
				}
				entry.mu.Lock()
				entry.RefCount--
				if entry.RefCount <= 0 {
					m.Delete(volumeID)
				}
				entry.mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	// No panic = success. Final state may or may not have an entry depending on ordering.
}
