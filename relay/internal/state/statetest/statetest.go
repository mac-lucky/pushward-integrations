// Package statetest provides test doubles for the state.Store interface, shared
// across relay provider handler tests.
package statetest

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
)

// ErrFailing is returned by every FailingStore and FailingMappingStore method.
var ErrFailing = errors.New("stub store failure")

// FailingStore is a state.Store whose every method returns ErrFailing. It drives
// handlers' best-effort store-degradation paths: a transient DB blip must not
// drop a brand-new alert.
type FailingStore struct{}

var _ state.Store = FailingStore{}

func (FailingStore) Set(context.Context, string, string, string, string, json.RawMessage, time.Duration) error {
	return ErrFailing
}

func (FailingStore) Get(context.Context, string, string, string, string) (json.RawMessage, error) {
	return nil, ErrFailing
}

func (FailingStore) GetGroup(context.Context, string, string, string) (map[string]json.RawMessage, error) {
	return nil, ErrFailing
}

func (FailingStore) Delete(context.Context, string, string, string, string) error { return ErrFailing }

func (FailingStore) DeleteGroup(context.Context, string, string, string) error { return ErrFailing }

func (FailingStore) Exists(context.Context, string, string, string, string) (bool, error) {
	return false, ErrFailing
}

func (FailingStore) Cleanup(context.Context) (int64, error) { return 0, ErrFailing }

// FailingMappingStore is a state.MappingStore whose every method returns
// ErrFailing. A mapping store outage must still deliver the webhook.
type FailingMappingStore struct{}

var _ state.MappingStore = FailingMappingStore{}

func (FailingMappingStore) Get(context.Context, state.MappingKey) (*state.MappingRow, error) {
	return nil, ErrFailing
}

func (FailingMappingStore) GetForDelivery(context.Context, state.MappingKey) (*state.MappingRow, error) {
	return nil, ErrFailing
}

func (FailingMappingStore) InsertPending(context.Context, *state.MappingRow, int) (bool, error) {
	return false, ErrFailing
}

func (FailingMappingStore) ReplaceStale(context.Context, *state.MappingRow, int, int) (bool, error) {
	return false, ErrFailing
}

func (FailingMappingStore) ClaimReview(context.Context, state.MappingKey) (bool, error) {
	return false, ErrFailing
}

func (FailingMappingStore) MarkReviewSent(context.Context, state.MappingKey) error { return ErrFailing }

func (FailingMappingStore) ReleaseReview(context.Context, state.MappingKey) error { return ErrFailing }

func (FailingMappingStore) Decide(context.Context, state.MappingKey, state.MappingStatus, time.Time, int) (state.DecideResult, error) {
	return state.DecideResult{}, ErrFailing
}

func (FailingMappingStore) Edit(context.Context, state.MappingKey, json.RawMessage, state.MappingStatus, int, int) (state.EditResult, error) {
	return state.EditResult{}, ErrFailing
}

func (FailingMappingStore) List(context.Context, [32]byte) ([]state.MappingRow, error) {
	return nil, ErrFailing
}

func (FailingMappingStore) Touch(context.Context, state.MappingKey) error { return ErrFailing }

func (FailingMappingStore) Sweep(context.Context) (state.SweepResult, error) {
	return state.SweepResult{}, ErrFailing
}

func (FailingMappingStore) CountByStatus(context.Context) (map[state.MappingStatus]int64, error) {
	return nil, ErrFailing
}
