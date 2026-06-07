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

// ErrFailing is returned by every FailingStore method.
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

func (FailingStore) ListByProvider(context.Context, string) ([]state.Entry, error) {
	return nil, ErrFailing
}

func (FailingStore) Cleanup(context.Context) (int64, error) { return 0, ErrFailing }
