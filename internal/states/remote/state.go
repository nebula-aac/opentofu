// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package remote

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sync"

	uuid "github.com/hashicorp/go-uuid"

	"github.com/opentofu/opentofu/internal/backend/local"
	"github.com/opentofu/opentofu/internal/encryption"
	"github.com/opentofu/opentofu/internal/states"
	"github.com/opentofu/opentofu/internal/states/statefile"
	"github.com/opentofu/opentofu/internal/states/statemgr"
	"github.com/opentofu/opentofu/internal/tofu"
)

// State implements the State interfaces in the state package to handle
// reading and writing the remote state. This State on its own does no
// local caching so every persist will go to the remote storage and local
// writes will go to memory.
type State struct {
	Client Client

	encryption encryption.StateEncryption

	// We track two pieces of meta data in addition to the state itself:
	//
	// lineage - the state's unique ID
	// serial  - the monotonic counter of "versions" of the state
	//
	// Both of these (along with state) have a sister field
	// that represents the values read in from an existing source.
	// All three of these values are used to determine if the new
	// state has changed from an existing state we read in.
	lineage, readLineage string
	serial, readSerial   uint64
	readEncryption       encryption.EncryptionStatus
	mu                   sync.Mutex
	state, readState     *states.State
	disableLocks         bool

	// If this is set then the state manager will decline to store intermediate
	// state snapshots created while a OpenTofu Core apply operation is in
	// progress. Otherwise (by default) it will accept persistent snapshots
	// using the default rules defined in the local backend.
	disableIntermediateSnapshots bool
}

var _ statemgr.Full = (*State)(nil)
var _ statemgr.Migrator = (*State)(nil)
var _ statemgr.PersistentMeta = (*State)(nil)
var _ local.IntermediateStateConditionalPersister = (*State)(nil)

func NewState(client Client, enc encryption.StateEncryption) *State {
	return &State{
		Client:     client,
		encryption: enc,
	}
}

func (s *State) DisableIntermediateSnapshots() {
	s.disableIntermediateSnapshots = true
}

// statemgr.Reader impl.
func (s *State) State() *states.State {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state.DeepCopy()
}

func (s *State) GetRootOutputValues(ctx context.Context) (map[string]*states.OutputValue, error) {
	if err := s.RefreshState(ctx); err != nil {
		return nil, fmt.Errorf("Failed to load state: %w", err)
	}

	state := s.State()
	if state == nil {
		state = states.NewState()
	}

	return state.RootModule().OutputValues, nil
}

// StateForMigration is part of our implementation of statemgr.Migrator.
func (s *State) StateForMigration() *statefile.File {
	s.mu.Lock()
	defer s.mu.Unlock()

	return statefile.New(s.state.DeepCopy(), s.lineage, s.serial)
}

// statemgr.Writer impl.
func (s *State) WriteState(state *states.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// We create a deep copy of the state here, because the caller also has
	// a reference to the given object and can potentially go on to mutate
	// it after we return, but we want the snapshot at this point in time.
	s.state = state.DeepCopy()

	return nil
}

// WriteStateForMigration is part of our implementation of statemgr.Migrator.
func (s *State) WriteStateForMigration(f *statefile.File, force bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !force {
		checkFile := statefile.New(s.state, s.lineage, s.serial)
		if err := statemgr.CheckValidImport(f, checkFile); err != nil {
			return err
		}
	}

	// The remote backend needs to pass the `force` flag through to its client.
	// For backends that support such operations, inform the client
	// that a force push has been requested
	c, isForcePusher := s.Client.(ClientForcePusher)
	if force && isForcePusher {
		c.EnableForcePush()
	}

	// We create a deep copy of the state here, because the caller also has
	// a reference to the given object and can potentially go on to mutate
	// it after we return, but we want the snapshot at this point in time.
	s.state = f.State.DeepCopy()
	s.lineage = f.Lineage
	s.serial = f.Serial

	return nil
}

// statemgr.Refresher impl.
func (s *State) RefreshState(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshState(ctx)
}

// refreshState is the main implementation of RefreshState, but split out so
// that we can make internal calls to it from methods that are already holding
// the s.mu lock.
func (s *State) refreshState(ctx context.Context) error {
	payload, err := s.Client.Get(ctx)
	if err != nil {
		return err
	}

	// no remote state is OK
	if payload == nil {
		s.readState = nil
		s.lineage = ""
		s.serial = 0
		return nil
	}

	stateFile, err := statefile.Read(bytes.NewReader(payload.Data), s.encryption)
	if err != nil {
		return err
	}

	s.lineage = stateFile.Lineage
	s.serial = stateFile.Serial
	s.state = stateFile.State

	// Properties from the remote must be separate so we can
	// track changes as lineage, serial and/or state are mutated
	s.readLineage = stateFile.Lineage
	s.readSerial = stateFile.Serial
	s.readEncryption = stateFile.EncryptionStatus
	s.readState = s.state.DeepCopy()
	return nil
}

// statemgr.Persister impl.
func (s *State) PersistState(ctx context.Context, schemas *tofu.Schemas) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	log.Printf("[DEBUG] states/remote: state read serial is: %d; serial is: %d", s.readSerial, s.serial)
	log.Printf("[DEBUG] states/remote: state read lineage is: %s; lineage is: %s", s.readLineage, s.lineage)

	if s.readState != nil {
		lineageUnchanged := s.readLineage != "" && s.lineage == s.readLineage
		serialUnchanged := s.readSerial != 0 && s.serial == s.readSerial
		stateUnchanged := statefile.StatesMarshalEqual(s.state, s.readState)
		if stateUnchanged && lineageUnchanged && serialUnchanged && s.readEncryption != encryption.StatusMigration {
			// If the state, lineage or serial haven't changed at all then we have nothing to do.
			return nil
		}
		s.serial++
	} else {
		// We might be writing a new state altogether, but before we do that
		// we'll check to make sure there isn't already a snapshot present
		// that we ought to be updating.
		err := s.refreshState(ctx)
		if err != nil {
			return fmt.Errorf("failed checking for existing remote state: %w", err)
		}
		log.Printf("[DEBUG] states/remote: after refresh, state read serial is: %d; serial is: %d", s.readSerial, s.serial)
		log.Printf("[DEBUG] states/remote: after refresh, state read lineage is: %s; lineage is: %s", s.readLineage, s.lineage)
		if s.lineage == "" { // indicates that no state snapshot is present yet
			lineage, err := uuid.GenerateUUID()
			if err != nil {
				return fmt.Errorf("failed to generate initial lineage: %w", err)
			}
			s.lineage = lineage
			s.serial++
		}
	}

	f := statefile.New(s.state, s.lineage, s.serial)

	var buf bytes.Buffer
	err := statefile.Write(f, &buf, s.encryption)
	if err != nil {
		return err
	}

	err = s.Client.Put(ctx, buf.Bytes())
	if err != nil {
		return err
	}

	// After we've successfully persisted, what we just wrote is our new
	// reference state until someone calls RefreshState again.
	// We've potentially overwritten (via force) the state, lineage
	// and / or serial (and serial was incremented) so we copy over all
	// three fields so everything matches the new state and a subsequent
	// operation would correctly detect no changes to the lineage, serial or state.
	s.readState = s.state.DeepCopy()
	s.readLineage = s.lineage
	s.readEncryption = encryption.StatusSatisfied
	s.readSerial = s.serial
	return nil
}

// ShouldPersistIntermediateState implements local.IntermediateStateConditionalPersister
func (s *State) ShouldPersistIntermediateState(info *local.IntermediateStatePersistInfo) bool {
	if s.disableIntermediateSnapshots {
		return false
	}
	return local.DefaultIntermediateStatePersistRule(info)
}

// Lock calls the Client's Lock method if it's implemented.
func (s *State) Lock(ctx context.Context, info *statemgr.LockInfo) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.disableLocks {
		return "", nil
	}

	if c, ok := s.Client.(ClientLocker); ok {
		return c.Lock(ctx, info)
	}
	return "", nil
}

// Unlock calls the Client's Unlock method if it's implemented.
func (s *State) Unlock(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.disableLocks {
		return nil
	}

	if c, ok := s.Client.(ClientLocker); ok {
		return c.Unlock(ctx, id)
	}
	return nil
}

func (s *State) IsLockingEnabled() bool {
	if s.disableLocks {
		return false
	}

	switch c := s.Client.(type) {
	// Client supports optional locking.
	case OptionalClientLocker:
		return c.IsLockingEnabled()
	// Client supports locking by default.
	case ClientLocker:
		return true
	// Client doesn't support any locking.
	default:
		return false
	}
}

// DisableLocks turns the Lock and Unlock methods into no-ops. This is intended
// to be called during initialization of a state manager and should not be
// called after any of the statemgr.Full interface methods have been called.
func (s *State) DisableLocks() {
	s.disableLocks = true
}

// StateSnapshotMeta returns the metadata from the most recently persisted
// or refreshed persistent state snapshot.
//
// This is an implementation of statemgr.PersistentMeta.
func (s *State) StateSnapshotMeta() statemgr.SnapshotMeta {
	return statemgr.SnapshotMeta{
		Lineage: s.lineage,
		Serial:  s.serial,
	}
}
