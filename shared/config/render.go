package config

// DefaultRenderConfig is the shipped default: pills off, live progress on.
//
// The zero value is not that default, so a bridge must start from here rather
// than from RenderConfig{} - otherwise its live progress silently ships off while
// the field's own doc says it defaults on.
func DefaultRenderConfig() RenderConfig {
	return RenderConfig{LiveProgress: true}
}

// RenderConfig gates the step-pill fields on the steps template. Both pill
// fields default to off, which reproduces the payload a bridge sent before they
// existed, so an operator opts in to the richer rendering. Construct it with
// DefaultRenderConfig, not as a zero value.
type RenderConfig struct {
	StepColors bool `yaml:"step_colors"`
	// StepWeights sizes each pill by how long that group ran in the prior run.
	// Reaching those durations can cost an extra lookup per seeded run on a forge
	// whose job objects carry no timestamps.
	StepWeights bool `yaml:"step_weights"`
	// LiveProgress lets iOS animate the current step's pill and count its ETA
	// down between polls, anchored to how long that job group ran in the prior
	// run. Unlike the pill fields it defaults on: the anchors are additive, so a
	// client that does not understand them renders the same bar as before.
	LiveProgress bool `yaml:"live_progress"`
}

// WantTimings reports whether anything downstream consumes the prior run's
// per-group durations. Both the pill sizing and the live-progress anchor read
// them, and gathering them can cost an extra API call, so a forge adapter checks
// this before paying for the join.
func (r RenderConfig) WantTimings() bool {
	return r.StepWeights || r.LiveProgress
}

// ApplyEnvOverrides applies the <prefix>_STEP_COLORS, <prefix>_STEP_WEIGHTS and
// <prefix>_LIVE_PROGRESS overrides. The prefix is the bridge's own, e.g.
// "PUSHWARD_GITHUB", so the variable names stay per-bridge while the parsing and
// its fail-loud behavior are shared.
func (r *RenderConfig) ApplyEnvOverrides(prefix string) error {
	if err := EnvBool(prefix+"_STEP_COLORS", &r.StepColors); err != nil {
		return err
	}
	if err := EnvBool(prefix+"_STEP_WEIGHTS", &r.StepWeights); err != nil {
		return err
	}
	return EnvBool(prefix+"_LIVE_PROGRESS", &r.LiveProgress)
}
