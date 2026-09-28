package ranker

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"

	"github.com/mac-lucky/pushward-integrations/relay/internal/universal"
)

// WeightsSchema is the weights.json layout this package reads.
const WeightsSchema = 1

// weights.json is written by the retrain job in pushward-classifier. It holds
// feature names built from field-name words and their coefficients, never a
// payload value.
//
//go:embed weights.json
var weightsJSON []byte

// load parses the embedded weights once, on first use.
var load = sync.OnceValues(func() (*Ranker, error) { return parse(weightsJSON) })

type weightsFile struct {
	Schema          int    `json:"schema"`
	FeaturesVersion int    `json:"features_version"`
	TopK            int    `json:"top_k"`
	ID              string `json:"id"`
	Gate            struct {
		Rule string `json:"rule"`
		Pass bool   `json:"pass"`
	} `json:"gate"`
	Serve map[string]bool                `json:"serve"`
	Roles map[universal.Role]roleWeights `json:"roles"`
	Kind  kindWeights                    `json:"kind"`
}

type roleWeights struct {
	Intercept   float64            `json:"intercept"`
	Temperature float64            `json:"temperature"`
	W           map[string]float64 `json:"w"`
}

type kindWeights struct {
	Classes     []universal.Kind     `json:"classes"`
	Intercept   []float64            `json:"intercept"`
	Temperature float64              `json:"temperature"`
	W           map[string][]float64 `json:"w"`
}

// serveKind is the serve key for the kind model.
const serveKind = "kind"

func parse(b []byte) (*Ranker, error) {
	var wf weightsFile
	if err := json.Unmarshal(b, &wf); err != nil {
		return nil, fmt.Errorf("ranker weights: %w", err)
	}
	var errs []error
	if wf.Schema != WeightsSchema {
		errs = append(errs, fmt.Errorf("schema %d, want %d", wf.Schema, WeightsSchema))
	}
	if wf.FeaturesVersion != FeaturesVersion {
		errs = append(errs, fmt.Errorf("features_version %d, want %d", wf.FeaturesVersion, FeaturesVersion))
	}
	if wf.TopK != TopK {
		errs = append(errs, fmt.Errorf("top_k %d, want %d", wf.TopK, TopK))
	}
	if wf.ID == "" {
		errs = append(errs, errors.New("no id"))
	}
	r := &Ranker{
		id:    wf.ID,
		by:    "ranker/" + wf.ID,
		pass:  wf.Gate.Pass,
		roles: make([]roleModel, len(universal.Roles)),
		serve: make([]bool, len(universal.Roles)),
	}
	for role := range wf.Roles {
		if !slices.Contains(universal.Roles, role) {
			errs = append(errs, fmt.Errorf("unknown role %q", role))
		}
	}
	for i, role := range universal.Roles {
		rw, ok := wf.Roles[role]
		if !ok {
			errs = append(errs, fmt.Errorf("no weights for %s", role))
			continue
		}
		if err := checkModel(string(role), rw.Temperature, []float64{rw.Intercept}, rw.W); err != nil {
			errs = append(errs, err)
		}
		r.roles[i] = roleModel{intercept: rw.Intercept, temp: rw.Temperature, w: rw.W}
	}
	for key, on := range wf.Serve {
		i := slices.Index(universal.Roles, universal.Role(key))
		switch {
		case key == serveKind:
			r.serveKind = on
		case i < 0:
			errs = append(errs, fmt.Errorf("serve: unknown key %q", key))
		default:
			r.serve[i] = on
		}
	}
	k := wf.Kind
	want := []universal.Kind{universal.KindAlert, universal.KindNotification, universal.KindProgress}
	sorted := slices.Clone(k.Classes)
	slices.Sort(sorted)
	if !slices.Equal(sorted, want) {
		errs = append(errs, fmt.Errorf("kind classes %v, want each of %v once", k.Classes, want))
	}
	if len(k.Intercept) != len(k.Classes) {
		errs = append(errs, fmt.Errorf("kind: %d intercepts for %d classes", len(k.Intercept), len(k.Classes)))
	}
	for name, w := range k.W {
		if len(w) != len(k.Classes) {
			errs = append(errs, fmt.Errorf("kind: %q has %d weights for %d classes", name, len(w), len(k.Classes)))
		}
		if slices.ContainsFunc(w, func(v float64) bool { return !finite(v) }) {
			errs = append(errs, fmt.Errorf("kind: weight %q is not finite", name))
		}
	}
	if err := checkModel(serveKind, k.Temperature, k.Intercept, nil); err != nil {
		errs = append(errs, err)
	}
	r.kind = kindModel{classes: k.Classes, intercept: k.Intercept, temp: k.Temperature, w: k.W}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("ranker weights %s: %w", wf.ID, err)
	}
	return r, nil
}

func checkModel(name string, temp float64, intercepts []float64, w map[string]float64) error {
	if !finite(temp) || temp <= 0 {
		return fmt.Errorf("%s: temperature %v", name, temp)
	}
	for _, b := range intercepts {
		if !finite(b) {
			return fmt.Errorf("%s: intercept %v", name, b)
		}
	}
	for f, v := range w {
		if !finite(v) {
			return fmt.Errorf("%s: weight %q is %v", name, f, v)
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
