package config

import (
	"fmt"
	"time"
)

const (
	// defaultIdleInterval is the shipped detection cadence. See IdleInterval for what
	// a pass costs and why raising this is the lever when a bridge gets throttled.
	defaultIdleInterval = 60 * time.Second
	// defaultActiveInterval caps the derived active tier. Kept well under the idle
	// default because the two tiers cost very different amounts: this one is a request
	// per *running* run, while the idle tier is a request per *watched repo*.
	defaultActiveInterval = 15 * time.Second
)

// DefaultPollingConfig is the shipped two-tier default.
//
// Interval is deliberately left unset rather than pre-filled: it is derived in
// ApplyActiveDefault, after the YAML and env layers have had their say. Pre-filling
// the cap here would turn an idle_interval below it into a cross-field error instead
// of following it down.
func DefaultPollingConfig() PollingConfig {
	return PollingConfig{IdleInterval: defaultIdleInterval}
}

// PollingConfig is the two-tier poll cadence the forge bridges share: a slow
// per-repo detection tier and a fast per-run update tier. Construct it with
// DefaultPollingConfig, then ApplyActiveDefault once every override layer has run.
type PollingConfig struct {
	// IdleInterval is how often every watched repo is checked for a run that has
	// just started. One request per repo per pass, so with a large owner - or an
	// instance where discovery finds a lot of repos - this is the bridge's whole idle
	// request rate: 44 repos at 30s is 5,280 requests an hour against GitHub's 5,000,
	// which is how this knob gets a bridge throttled.
	IdleInterval time.Duration `yaml:"idle_interval"`
	// Interval is how often a run already being tracked is advanced - what someone
	// watching the card sees. One request per run in flight, so it stays cheap no
	// matter how many repos are watched. Derived from idle_interval when unset; see
	// ApplyActiveDefault.
	Interval time.Duration `yaml:"interval"`
}

// ApplyEnvOverrides applies PUSHWARD_POLL_IDLE and PUSHWARD_POLL_INTERVAL.
//
// The names take no bridge prefix, unlike RenderConfig's: both forge bridges have
// always read these exact two variables, and a deployment that sets them means them
// for whichever bridge is in the container. Adding a prefix would silently ignore
// every manifest already in production.
func (p *PollingConfig) ApplyEnvOverrides() error {
	if err := EnvDuration("PUSHWARD_POLL_IDLE", &p.IdleInterval); err != nil {
		return err
	}
	return EnvDuration("PUSHWARD_POLL_INTERVAL", &p.Interval)
}

// ApplyActiveDefault derives an unset active tier: the smaller of idle_interval and
// defaultActiveInterval. Call it after the last override layer, so an operator who
// lowered idle_interval to get faster cards is not left with an active tier slower
// than the idle one.
//
// Only an unset value is derived. Anything else - including a value slower than the
// idle tier - is left for Validate to reject rather than quietly repaired: clamping
// it would hand the operator back a cadence they never configured.
func (p *PollingConfig) ApplyActiveDefault() {
	if p.Interval == 0 {
		p.Interval = min(p.IdleInterval, defaultActiveInterval)
	}
}

// Validate rejects a cadence the poll loop cannot run on: a non-positive interval
// panics time.NewTicker, a non-positive idle_interval makes every tick a full
// detection pass and burns the forge's whole request budget, and an active tier
// slower than the idle one would stretch detection to interval and leave the
// configured idle_interval meaningless.
//
// The errors name the YAML key rather than the Go field. Most readers are operators
// editing a manifest; an adapter author building Options by hand gets the same
// spelling, which cipoll's wrap keeps traceable.
func (p *PollingConfig) Validate() error {
	if p.IdleInterval <= 0 {
		return fmt.Errorf("polling.idle_interval must be positive, got %s", p.IdleInterval)
	}
	if p.Interval <= 0 {
		return fmt.Errorf("polling.interval must be positive, got %s", p.Interval)
	}
	if p.Interval > p.IdleInterval {
		return fmt.Errorf("polling.interval (%s) must not exceed polling.idle_interval (%s): the active tier cannot be slower than the idle one",
			p.Interval, p.IdleInterval)
	}
	return nil
}
