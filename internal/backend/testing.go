// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package backend

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	uuid "github.com/hashicorp/go-uuid"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hcldec"

	"github.com/opentofu/opentofu/internal/addrs"
	"github.com/opentofu/opentofu/internal/configs"
	"github.com/opentofu/opentofu/internal/legacy/hcl2shim"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/tfdiags"
)

func separateWarningsAndErrors(diags tfdiags.Diagnostics) ([]string, []error) {
	warnings := make([]string, 0)
	errors := make([]error, 0)
	for _, diag := range diags {
		if diag.Severity() == tfdiags.Warning {
			warnings = append(warnings, diag.Description().Summary)
		} else if diag.Severity() == tfdiags.Error {
			errors = append(errors, fmt.Errorf("%s", diag.Description().Summary))
		}
	}
	return warnings, errors
}

// TestBackendConfigWarningsAndErrors validates and configures the backend with the
// given configuration and returns backend and any warnings and errors encountered.
// used to test validations
func TestBackendConfigWarningsAndErrors(t *testing.T, b Backend, c hcl.Body) (Backend, []string, []error) {
	t.Helper()

	t.Logf("TestBackendConfig on %T with %#v", b, c)

	var diags tfdiags.Diagnostics

	// To make things easier for test authors, we'll allow a nil body here
	// (even though that's not normally valid) and just treat it as an empty
	// body.
	if c == nil {
		c = hcl.EmptyBody()
	}

	schema := b.ConfigSchema()
	spec := schema.DecoderSpec()
	obj, decDiags := hcldec.Decode(c, spec, nil)
	diags = diags.Append(decDiags)

	newObj, valDiags := b.PrepareConfig(obj)
	diags = diags.Append(valDiags.InConfigBody(c, ""))

	// it's valid for a Backend to have warnings (e.g. a Deprecation) as such we should only raise on errors
	if len(diags) != 0 {
		warnings, errors := separateWarningsAndErrors(diags)
		return nil, warnings, errors
	}

	obj = newObj

	confDiags := b.Configure(t.Context(), obj)
	if len(confDiags) != 0 {
		confDiags = confDiags.InConfigBody(c, "")
		warnings, errors := separateWarningsAndErrors(confDiags)
		return nil, warnings, errors
	}

	return b, nil, nil
}

// TestBackendConfig validates and configures the backend with the
// given configuration.
func TestBackendConfig(t *testing.T, b Backend, c hcl.Body) Backend {
	t.Helper()

	t.Logf("TestBackendConfig on %T with %#v", b, c)

	var diags tfdiags.Diagnostics

	// To make things easier for test authors, we'll allow a nil body here
	// (even though that's not normally valid) and just treat it as an empty
	// body.
	if c == nil {
		c = hcl.EmptyBody()
	}

	schema := b.ConfigSchema()
	spec := schema.DecoderSpec()
	obj, decDiags := hcldec.Decode(c, spec, nil)
	diags = diags.Append(decDiags)

	newObj, valDiags := b.PrepareConfig(obj)
	diags = diags.Append(valDiags.InConfigBody(c, ""))

	// it's valid for a Backend to have warnings (e.g. a Deprecation) as such we should only raise on errors
	if diags.HasErrors() {
		t.Fatal(diags.ErrWithWarnings())
	}

	obj = newObj

	confDiags := b.Configure(t.Context(), obj)
	if len(confDiags) != 0 {
		confDiags = confDiags.InConfigBody(c, "")
		t.Fatal(confDiags.ErrWithWarnings())
	}

	return b
}

// TestWrapConfig takes a raw data structure and converts it into a
// synthetic hcl.Body to use for testing.
//
// The given structure should only include values that can be accepted by
// hcl2shim.HCL2ValueFromConfigValue. If incompatible values are given,
// this function will panic.
func TestWrapConfig(raw map[string]interface{}) hcl.Body {
	obj := hcl2shim.HCL2ValueFromConfigValue(raw)
	return configs.SynthBody("<TestWrapConfig>", obj.AsValueMap())
}

// TestBackend will test the functionality of a Backend. The backend is
// assumed to already be configured. This will test state functionality.
// If the backend reports it doesn't support multi-state by returning the
// error ErrWorkspacesNotSupported, then it will not test that.
func TestBackendStates(t *testing.T, b Backend) {
	t.Helper()

	noDefault := false
	if _, err := b.StateMgr(t.Context(), DefaultStateName); err != nil {
		if err == ErrDefaultWorkspaceNotSupported {
			noDefault = true
		} else {
			t.Fatalf("error: %v", err)
		}
	}

	workspaces, err := b.Workspaces(t.Context())
	if err != nil {
		if err == ErrWorkspacesNotSupported {
			t.Logf("TestBackend: workspaces not supported in %T, skipping", b)
			return
		}
		t.Fatalf("error: %v", err)
	}

	// Test it starts with only the default
	if !noDefault && (len(workspaces) != 1 || workspaces[0] != DefaultStateName) {
		t.Fatalf("should only have the default workspace to start: %#v", workspaces)
	}

	// Create a couple states
	foo, err := b.StateMgr(t.Context(), "foo")
	if err != nil {
		t.Fatalf("error: %s", err)
	}
	if err := foo.RefreshState(t.Context()); err != nil {
		t.Fatalf("bad: %s", err)
	}
	if v := foo.State(); v.HasManagedResourceInstanceObjects() {
		t.Fatalf("should be empty: %s", v)
	}

	bar, err := b.StateMgr(t.Context(), "bar")
	if err != nil {
		t.Fatalf("error: %s", err)
	}
	if err := bar.RefreshState(t.Context()); err != nil {
		t.Fatalf("bad: %s", err)
	}
	if v := bar.State(); v.HasManagedResourceInstanceObjects() {
		t.Fatalf("should be empty: %s", v)
	}

	// Verify they are distinct states that can be read back from storage
	{
		// We'll use two distinct states here and verify that changing one
		// does not also change the other.
		fooState := states.NewState()
		barState := states.NewState()

		// write a known state to foo
		if err := foo.WriteState(fooState); err != nil {
			t.Fatal("error writing foo state:", err)
		}
		if err := foo.PersistState(t.Context(), nil); err != nil {
			t.Fatal("error persisting foo state:", err)
		}

		// We'll make "bar" different by adding a fake resource state to it.
		barState.SyncWrapper().SetResourceInstanceCurrent(
			addrs.ResourceInstance{
				Resource: addrs.Resource{
					Mode: addrs.ManagedResourceMode,
					Type: "test_thing",
					Name: "foo",
				},
			}.Absolute(addrs.RootModuleInstance),
			&states.ResourceInstanceObjectSrc{
				AttrsJSON:     []byte("{}"),
				Status:        states.ObjectReady,
				SchemaVersion: 0,
			},
			addrs.AbsProviderConfig{
				Provider: addrs.NewDefaultProvider("test"),
				Module:   addrs.RootModule,
			},
			addrs.NoKey,
		)

		// write a distinct known state to bar
		if err := bar.WriteState(barState); err != nil {
			t.Fatalf("bad: %s", err)
		}
		if err := bar.PersistState(t.Context(), nil); err != nil {
			t.Fatalf("bad: %s", err)
		}

		// verify that foo is unchanged with the existing state manager
		if err := foo.RefreshState(t.Context()); err != nil {
			t.Fatal("error refreshing foo:", err)
		}
		fooState = foo.State()
		if fooState.HasManagedResourceInstanceObjects() {
			t.Fatal("after writing a resource to bar, foo now has resources too")
		}

		// fetch foo again from the backend
		foo, err = b.StateMgr(t.Context(), "foo")
		if err != nil {
			t.Fatal("error re-fetching state:", err)
		}
		if err := foo.RefreshState(t.Context()); err != nil {
			t.Fatal("error refreshing foo:", err)
		}
		fooState = foo.State()
		if fooState.HasManagedResourceInstanceObjects() {
			t.Fatal("after writing a resource to bar and re-reading foo, foo now has resources too")
		}

		// fetch the bar again from the backend
		bar, err = b.StateMgr(t.Context(), "bar")
		if err != nil {
			t.Fatal("error re-fetching state:", err)
		}
		if err := bar.RefreshState(t.Context()); err != nil {
			t.Fatal("error refreshing bar:", err)
		}
		barState = bar.State()
		if !barState.HasManagedResourceInstanceObjects() {
			t.Fatal("after writing a resource instance object to bar and re-reading it, the object has vanished")
		}
	}

	// Verify we can now list them
	{
		// we determined that named stated are supported earlier
		workspaces, err := b.Workspaces(t.Context())
		if err != nil {
			t.Fatalf("err: %s", err)
		}

		sort.Strings(workspaces)
		expected := []string{"bar", "default", "foo"}
		if noDefault {
			expected = []string{"bar", "foo"}
		}
		if !reflect.DeepEqual(workspaces, expected) {
			t.Fatalf("wrong workspaces list\ngot:  %#v\nwant: %#v", workspaces, expected)
		}
	}

	// Delete some workspaces
	if err := b.DeleteWorkspace(t.Context(), "foo", true); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Verify the default state can't be deleted
	if err := b.DeleteWorkspace(t.Context(), DefaultStateName, true); err == nil {
		t.Fatal("expected error")
	}

	// Create and delete the foo workspace again.
	// Make sure that there are no leftover artifacts from a deleted state
	// preventing re-creation.
	foo, err = b.StateMgr(t.Context(), "foo")
	if err != nil {
		t.Fatalf("error: %s", err)
	}
	if err := foo.RefreshState(t.Context()); err != nil {
		t.Fatalf("bad: %s", err)
	}
	if v := foo.State(); v.HasManagedResourceInstanceObjects() {
		t.Fatalf("should be empty: %s", v)
	}
	// and delete it again
	if err := b.DeleteWorkspace(t.Context(), "foo", true); err != nil {
		t.Fatalf("err: %s", err)
	}

	// Verify deletion
	{
		workspaces, err := b.Workspaces(t.Context())
		if err != nil {
			t.Fatalf("err: %s", err)
		}

		sort.Strings(workspaces)
		expected := []string{"bar", "default"}
		if noDefault {
			expected = []string{"bar"}
		}
		if !reflect.DeepEqual(workspaces, expected) {
			t.Fatalf("wrong workspaces list\ngot:  %#v\nwant: %#v", workspaces, expected)
		}
	}
}

// TestBackendStateLocks will test the locking functionality of the remote
// state backend.
func TestBackendStateLocks(t *testing.T, b1, b2 Backend) {
	t.Helper()
	testLocks(t, b1, b2, false)
}

// TestBackendStateForceUnlock verifies that the lock error is the expected
// type, and the lock can be unlocked using the ID reported in the error.
// Remote state backends that support -force-unlock should call this in at
// least one of the acceptance tests.
func TestBackendStateForceUnlock(t *testing.T, b1, b2 Backend) {
	t.Helper()
	testLocks(t, b1, b2, true)
}

// TestBackendStateLocksInWS will test the locking functionality of the remote
// state backend.
func TestBackendStateLocksInWS(t *testing.T, b1, b2 Backend, ws string) {
	t.Helper()
	testLocksInWorkspace(t, b1, b2, false, ws)
}

// TestBackendStateForceUnlockInWS verifies that the lock error is the expected
// type, and the lock can be unlocked using the ID reported in the error.
// Remote state backends that support -force-unlock should call this in at
// least one of the acceptance tests.
func TestBackendStateForceUnlockInWS(t *testing.T, b1, b2 Backend, ws string) {
	t.Helper()
	testLocksInWorkspace(t, b1, b2, true, ws)
}

func testLocks(t *testing.T, b1, b2 Backend, testForceUnlock bool) {
	testLocksInWorkspace(t, b1, b2, testForceUnlock, DefaultStateName)
}

func testLocksInWorkspace(t *testing.T, b1, b2 Backend, testForceUnlock bool, workspace string) {
	t.Helper()

	// Get the default state for each
	b1StateMgr, err := b1.StateMgr(t.Context(), DefaultStateName)
	if err != nil {
		t.Fatalf("error: %s", err)
	}
	if err := b1StateMgr.RefreshState(t.Context()); err != nil {
		t.Fatalf("bad: %s", err)
	}

	// Fast exit if this doesn't support locking at all
	if _, ok := b1StateMgr.(statemgr.Locker); !ok {
		t.Logf("TestBackend: backend %T doesn't support state locking, not testing", b1)
		return
	}

	t.Logf("TestBackend: testing state locking for %T", b1)

	b2StateMgr, err := b2.StateMgr(t.Context(), DefaultStateName)
	if err != nil {
		t.Fatalf("error: %s", err)
	}
	if err := b2StateMgr.RefreshState(t.Context()); err != nil {
		t.Fatalf("bad: %s", err)
	}

	// Reassign so its obvious whats happening
	lockerA := b1StateMgr.(statemgr.Locker)
	lockerB := b2StateMgr.(statemgr.Locker)

	infoA := statemgr.NewLockInfo()
	infoA.Operation = "test"
	infoA.Who = "clientA"

	infoB := statemgr.NewLockInfo()
	infoB.Operation = "test"
	infoB.Who = "clientB"

	lockIDA, err := lockerA.Lock(t.Context(), infoA)
	if err != nil {
		t.Fatal("unable to get initial lock:", err)
	}

	// Make sure we can still get the statemgr.Full from another instance even
	// when locked.  This should only happen when a state is loaded via the
	// backend, and as a remote state.
	_, err = b2.StateMgr(t.Context(), DefaultStateName)
	if err != nil {
		t.Errorf("failed to read locked state from another backend instance: %s", err)
	}

	// If the lock ID is blank, assume locking is disabled
	if lockIDA == "" {
		t.Logf("TestBackend: %T: empty string returned for lock, assuming disabled", b1)
		return
	}

	_, err = lockerB.Lock(t.Context(), infoB)
	if err == nil {
		_ = lockerA.Unlock(t.Context(), lockIDA) // test already failed, no need to check err further
		t.Fatal("client B obtained lock while held by client A")
	}

	if err := lockerA.Unlock(t.Context(), lockIDA); err != nil {
		t.Fatal("error unlocking client A", err)
	}

	lockIDB, err := lockerB.Lock(t.Context(), infoB)
	if err != nil {
		t.Fatal("unable to obtain lock from client B")
	}

	if lockIDB == lockIDA {
		t.Errorf("duplicate lock IDs: %q", lockIDB)
	}

	if err = lockerB.Unlock(t.Context(), lockIDB); err != nil {
		t.Fatal("error unlocking client B:", err)
	}

	// test the equivalent of -force-unlock, by using the id from the error
	// output.
	if !testForceUnlock {
		return
	}

	// get a new ID
	infoA.ID, err = uuid.GenerateUUID()
	if err != nil {
		panic(err)
	}

	lockIDA, err = lockerA.Lock(t.Context(), infoA)
	if err != nil {
		t.Fatal("unable to get re lock A:", err)
	}
	unlock := func() {
		err := lockerA.Unlock(t.Context(), lockIDA)
		if err != nil {
			t.Fatal(err)
		}
	}

	_, err = lockerB.Lock(t.Context(), infoB)
	if err == nil {
		unlock()
		t.Fatal("client B obtained lock while held by client A")
	}

	infoErr, ok := err.(*statemgr.LockError)
	if !ok {
		unlock()
		t.Fatalf("expected type *statemgr.LockError, got : %#v", err)
	}

	// try to unlock with the second unlocker, using the ID from the error
	if err := lockerB.Unlock(t.Context(), infoErr.Info.ID); err != nil {
		unlock()
		t.Fatalf("could not unlock with the reported ID %q: %s", infoErr.Info.ID, err)
	}
}
