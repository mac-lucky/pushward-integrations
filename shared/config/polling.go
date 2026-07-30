package config

import (
	"fmt"
	"time"
)

const (
	// defaultIdleInterval is the gap between detection passes over every watched repo.
	defaultIdleInterval = 60 * time.Second
	// defaultActiveInterval is the gap between updates to a run already in flight
	// when polling.interval is not set. Kept well under the idle default because the
	// two tiers cost very different amounts: this one is a request per *running* run,
	// while the idle tier is a request per *watched repo*.
	defaultActiveInterval = 15 * time.Second
)

// DefaultPollingConfig is the shipped two-tier default.
//
// Interval is deliberately left unset rather than pre-filled with 15s: it inherits
// from IdleInterval in ApplyActiveDefault, after the YAML and env layers have had
// their say. Pre-filling it here would reject an idle_interval of 10s as a
// cross-field error instead of following it down.
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
	// matter how many repos are watched. Defaults to the smaller of idle_interval
	// and 15s, so lowering idle_interval alone never leaves this slower than it.
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

// ApplyActiveDefault fills in an unset active tier from the idle one. Call it after
// the last override layer, so an operator who lowered idle_interval to get faster
// cards is not left with an active tier slower than the idle one.
//
// Only an unset value inherits: a negative one is a mistake and falls through to
// Validate rather than being quietly turned into something workable.
func (p *PollingConfig) ApplyActiveDefault() {
	if p.Interval == 0 {
		p.Interval = min(p.IdleInterval, defaultActiveInterval)
	}
}

// Validate rejects a cadence the poll loop cannot run on. A non-positive interval
// would panic time.NewTicker, and an active tier slower than the idle one is
// nonsense the loop cannot honor.
//
// The errors name the YAML key rather than the Go field: the reader is the operator
// whose manifest set it, and both this layer and cipoll surface them through the
// same startup log.
func (p PollingConfig) Validate() error {
	if p.IdleInterval <= 0 {
		return fmt.Errorf("polling.idle_interval must be positive, got %s", p.IdleInterval)
	}
	if p.Interval <= 0 {
		return fmt.Errorf("polling.interval must be positive, got %s", p.Interval)
	}
	// The loop ticks on interval and gates the detection pass off idle_interval, so
	// the fast tier being the slower of the two would stretch detection to interval
	// and leave the configured idle_interval meaningless.
	if p.Interval > p.IdleInterval {
		return fmt.Errorf("polling.interval (%s) must not exceed polling.idle_interval (%s): the active tier cannot be slower than the idle one",
			p.Interval, p.IdleInterval)
	}
	return nil
}
