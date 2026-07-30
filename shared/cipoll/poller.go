package cipoll

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/mac-lucky/pushward-integrations/shared/ci"
	sharedconfig "github.com/mac-lucky/pushward-integrations/shared/config"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/syncx"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

const repoRefreshInterval = 5 * time.Minute

// maxRunLifetime caps how long a single run is tracked. It reclaims runs stuck
// in progress (e.g. a hung self-hosted runner) that would otherwise block
// new-run detection for the repo indefinitely. Set well above any realistic job
// timeout ceiling so legitimate long runs are never evicted prematurely.
const maxRunLifetime = 12 * time.Hour

// defaultIcon is the SF Symbol on the card when Options leaves Icon empty.
const defaultIcon = "arrow.triangle.branch"

// SlugHashLen is the number of hash bytes appended to the slug prefix. It is
// observable contract, not a tuning knob: changing it renames every activity slug
// and orphans the cards already on people's lock screens.
const SlugHashLen = 4

// endPushTimeout bounds one end-phase delivery. The phases run on a detached
// context, so without this a hung connection would hold the timer goroutine open
// past shutdown.
const endPushTimeout = 30 * time.Second

// staleEvictionGrace is how long past the point an activity could be dismissed
// server-side we keep waiting before concluding the run vanished.
const staleEvictionGrace = 30 * time.Second

// titleLimit and subtitleLimit keep the card strings inside the server's 256-rune
// caps. The client fails fast on 4xx, so an over-long workflow name would fail
// the create outright and leave that repo showing nothing at all.
const (
	titleLimit    = 100
	subtitleLimit = 120
)

// Options configures a Poller. Everything here is static for the process
// lifetime; the forge-specific behavior lives behind Forge instead.
type Options struct {
	// Owner, when set, is enumerated every repoRefreshInterval and merged with
	// Repos. Empty disables discovery.
	Owner string
	// Repos are watched in addition to whatever Owner discovers.
	Repos []string
	// IdleInterval is how often the loop polls.
	IdleInterval time.Duration

	PushWard sharedconfig.PushWardConfig
	Render   sharedconfig.RenderConfig

	// TitlePrefix names the forge in the activity title: "GitHub" renders as
	// "GitHub: app".
	TitlePrefix string
	// SlugPrefix namespaces the per-repo activity slug. Two bridges watching the
	// same repos must not share one, or they contend for a single activity.
	SlugPrefix string
	// Icon is the SF Symbol on the card. Both bridges today take the default; the
	// field exists so a forge that wants its own glyph does not have to touch this
	// package.
	Icon string

	// DiscoveryRequired makes a failed *initial* owner enumeration fatal, taking
	// the bridge down rather than polling nothing. A caller with no explicit repo
	// list has nothing to fall back to and should set it; one with a configured
	// list should not, so a forge that is briefly unreachable at boot does not
	// crashloop the bridge. It has no effect when Owner is empty, since discovery
	// never runs then.
	DiscoveryRequired bool

	// Logger receives everything the loop reports, defaulting to slog.Default().
	// Injected rather than reached for globally so importing this package cannot
	// change a bridge's logging, and two bridges in one process stay separable.
	Logger *slog.Logger
}

// Poller tracks one activity per repo, replacing it as new runs start.
type Poller struct {
	forge Forge
	pw    *pushward.Client
	opts  Options
	log   *slog.Logger

	// mu guards every field below it, including repos and lastRefresh: Run drives
	// one goroutine today, but Poll is exported and a caller driving it alongside
	// Run would otherwise tear the slice header.
	mu          sync.Mutex
	tracked     map[string]*trackedRun
	repos       []string
	lastRefresh time.Time
}

func New(forge Forge, pw *pushward.Client, opts Options) *Poller {
	if opts.Icon == "" {
		opts.Icon = defaultIcon
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{
		forge:   forge,
		pw:      pw,
		opts:    opts,
		log:     logger,
		tracked: make(map[string]*trackedRun),
		repos:   opts.Repos,
	}
}

// validate rejects the Options a forge adapter can get wrong in a way the loop
// cannot recover from. IdleInterval would panic time.NewTicker; the two prefixes
// participate in the activity's identity, so a blank one is not a cosmetic
// problem but a wrong slug persisted per repo.
func (p *Poller) validate() error {
	if p.opts.IdleInterval <= 0 {
		return fmt.Errorf("cipoll: IdleInterval must be positive, got %s", p.opts.IdleInterval)
	}
	if p.opts.SlugPrefix == "" {
		return fmt.Errorf("cipoll: SlugPrefix is required, it namespaces the activity slug")
	}
	if p.opts.TitlePrefix == "" {
		return fmt.Errorf("cipoll: TitlePrefix is required, it names the forge on the card")
	}
	return nil
}

func (p *Poller) Run(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}
	defer p.drainEndTimers()

	if err := p.refreshRepos(ctx); err != nil {
		if p.opts.DiscoveryRequired {
			return fmt.Errorf("initial repo discovery: %w", err)
		}
		p.log.Error("repo discovery failed, continuing with the configured repos",
			"repos", p.opts.Repos, "error", err)
	}

	// First check immediately on startup.
	if err := p.Poll(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		p.log.Error("initial poll error", "error", err)
	}

	ticker := time.NewTicker(p.opts.IdleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}

		if err := p.refreshRepos(ctx); err != nil {
			p.log.Error("repo refresh failed", "error", err)
		}

		if err := p.Poll(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			p.log.Error("poll error", "error", err)
		}
	}
}

// drainEndTimers lets in-flight end phases finish on shutdown.
//
// The groups are collected under the lock, then Closed and Waited on OUTSIDE it:
// a phase callback re-acquires p.mu, so waiting under the lock would deadlock.
// Close (not Stop) is what stops a phase-1 callback re-arming phase 2 behind us,
// and every group is Closed before any is Waited on so a callback already running
// cannot arm a phase we have moved past.
func (p *Poller) drainEndTimers() {
	p.mu.Lock()
	groups := make([]*syncx.TimerGroup, 0, len(p.tracked))
	for _, t := range p.tracked {
		if t.endTimers != nil {
			groups = append(groups, t.endTimers)
		}
	}
	p.mu.Unlock()
	for _, g := range groups {
		g.Close()
	}
	for _, g := range groups {
		g.Wait()
	}
}

func (p *Poller) refreshRepos(ctx context.Context) error {
	if p.opts.Owner == "" {
		return nil
	}
	p.mu.Lock()
	fresh := !p.lastRefresh.IsZero() && time.Since(p.lastRefresh) < repoRefreshInterval
	p.mu.Unlock()
	if fresh {
		return nil
	}

	// Deliberately outside the lock: every other critical section in this file is
	// short and never spans network I/O, and enumerating an owner's repos is the
	// slowest call the bridge makes.
	discovered, err := p.forge.ListRepos(ctx, p.opts.Owner)
	if err != nil {
		return err
	}

	// Merge: discovered repos + any explicitly configured repos
	seen := make(map[string]bool, len(discovered)+len(p.opts.Repos))
	var merged []string
	for _, r := range discovered {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}
	for _, r := range p.opts.Repos {
		if !seen[r] {
			seen[r] = true
			merged = append(merged, r)
		}
	}

	p.mu.Lock()
	changed := len(merged) != len(p.repos)
	p.repos = merged
	p.lastRefresh = time.Now()
	p.mu.Unlock()
	if changed {
		p.log.Info("repo list updated", "count", len(merged))
	}
	return nil
}

// watched is the repo list to poll this cycle. The slice itself is never mutated
// in place - refreshRepos replaces it wholesale - so a header copy is enough.
func (p *Poller) watched() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.repos
}

// Poll runs one cycle: pick up newly started runs, then advance the ones already
// tracked. Run calls it on the idle interval, and it is exported so a bridge can
// drive a single cycle - which is what its end-to-end test does.
//
// Not safe to call concurrently with Run, or with itself: the per-repo state is
// guarded, but two cycles in flight would both poll every repo and double the
// forge's request rate.
func (p *Poller) Poll(ctx context.Context) error {
	if err := p.pollIdle(ctx); err != nil {
		return err
	}
	return p.pollActive(ctx)
}

func (p *Poller) pollIdle(ctx context.Context) error {
	for _, repo := range p.watched() {
		// Skip repos that already have an active entry (no pending end).
		p.mu.Lock()
		existing, ok := p.tracked[repo]
		if ok && existing.endTimers == nil {
			p.mu.Unlock()
			continue
		}
		// Snapshot the pending-end run id (if any). We do NOT cancel the pending
		// end yet - only once a genuinely new run is confirmed below - so a
		// pollIdle tick can't drop the completion frames when no replacement run
		// exists (which happens when EndDelay+EndDisplayTime >= IdleInterval).
		pendingRunID := int64(-1)
		if ok && existing.endTimers != nil {
			pendingRunID = existing.RunID
		}
		p.mu.Unlock()

		runs, err := p.forge.ActiveRuns(ctx, repo)
		if err != nil {
			p.log.Error("failed to get runs", "repo", repo, "error", err)
			continue
		}
		if len(runs) == 0 {
			continue // leave any pending end intact
		}

		// Pick the most recently created run
		run := runs[0]
		for _, r := range runs[1:] {
			if r.CreatedAt.After(run.CreatedAt) {
				run = r
			}
		}

		// A pending end belongs to an already-completed run; only supersede it
		// once we've confirmed a different in-progress run exists.
		if pendingRunID != -1 {
			if run.ID == pendingRunID {
				continue // same run, keep its pending completion frames
			}
			p.mu.Lock()
			if cur, ok := p.tracked[repo]; ok && cur.endTimers != nil && cur.RunID == pendingRunID {
				// Close (terminal), not Stop: the superseding run gets a fresh
				// TimerGroup, and Stop is non-terminal - an in-flight phase-1
				// callback could still re-arm phase-2 in the window between its own
				// unlock and Reset, sending a stale ENDED to the new run's
				// (repo-derived, shared) slug. Close makes re-arm a no-op.
				cur.endTimers.Close()
				delete(p.tracked, repo)
				p.log.Info("cancelled pending end for new workflow", "repo", repo, "slug", cur.Slug)
			}
			p.mu.Unlock()
		}

		repoShort := repoName(repo)
		slug := text.SlugHash(p.opts.SlugPrefix, repo, SlugHashLen)

		p.log.Info("workflow found", p.runAttrs(repo, run, slug)...)

		// Create the activity in PushWard
		endedTTL := int(p.opts.PushWard.CleanupDelay.Seconds())
		staleTTL := int(p.opts.PushWard.StaleTimeout.Seconds())
		title := text.TruncateHard(fmt.Sprintf("%s: %s", p.opts.TitlePrefix, repoShort), titleLimit)
		if err := p.pw.CreateActivity(ctx, slug, title, p.opts.PushWard.Priority, endedTTL, staleTTL); err != nil {
			p.log.Error("failed to create activity", "slug", slug, "error", err)
			continue
		}

		p.mu.Lock()
		// Guard against a concurrent phase-2 callback that may have inserted a
		// newer entry while we were doing network I/O without the lock.
		if cur, ok := p.tracked[repo]; ok && cur.endTimers == nil {
			p.mu.Unlock()
			continue
		}
		p.tracked[repo] = &trackedRun{
			RunID:      run.ID,
			Name:       run.Name,
			Slug:       slug,
			HTMLURL:    run.HTMLURL,
			RepoURL:    run.RepoURL,
			LastUpdate: time.Now(),
			trackedAt:  time.Now(),
			// Assume an animation is running until proven otherwise. The slug is
			// per-repo, so the card may still hold the previous run's window, and
			// the seed below is what clears it - but the seed can fail. Starting
			// pessimistic means the first tick that has nothing to animate sends
			// the clear itself instead of trusting a frame that may never have
			// landed.
			liveSent: p.opts.Render.LiveProgress,
		}
		p.mu.Unlock()

		// Determine the initial step shape. Forges create jobs lazily within a
		// run (jobs gated by needs/if appear only after their deps finish), so a
		// fresh scan sees just the first wave and the denominator would climb
		// (1/2 -> 3/4 -> 5/6). Seed from a prior run of the same workflow+branch,
		// which already revealed its full DAG, for a stable total from frame 1.
		shape := ci.StepInfo{TotalSteps: 1}
		if jobs, err := p.forge.LiveJobs(ctx, repo, run.ID); err != nil {
			p.log.Warn("failed to fetch jobs for initial step count, using default",
				"repo", repo, "run_id", run.ID, "error", err)
		} else if len(jobs) > 0 {
			shape = p.shape(jobs)
			p.log.Info("initial job scan",
				"repo", repo, "jobs", len(jobs),
				"steps", shape.TotalSteps, "step_rows", shape.StepRows)
		}
		// Adopt the prior run's shape wholesale when it has MORE step-groups than
		// the current scan (the prior full run, or a current run that has since
		// grown). Choosing one coherent shape - not an element-wise merge - keeps
		// step_labels consistent with current_step's index into them.
		var weightsByName map[string]float64
		if base, ok := p.baselineShape(ctx, repo, run); ok {
			// Pill durations are keyed by group name, so they attach correctly
			// whether we adopt the prior shape wholesale or keep the live one.
			weightsByName = base.WeightsByName
			if base.TotalSteps > shape.TotalSteps {
				shape = base
			}
		}
		initialTotalSteps := shape.TotalSteps
		initialStepRows := shape.StepRows
		initialStepLabels := shape.StepLabels
		initialStepColors := shape.StepColors
		initialStepWeights := p.payloadWeights(initialStepLabels, weightsByName)

		p.mu.Lock()
		if t, ok := p.tracked[repo]; ok {
			t.maxTotalSteps = initialTotalSteps
			t.maxStepRows = append([]int(nil), initialStepRows...)
			t.maxStepLabels = append([]string(nil), initialStepLabels...)
			t.maxStepColors = append([]string(nil), initialStepColors...)
			// Always assign a whole fresh map, never mutate one already stored:
			// readers copy this reference out and then read the contents after
			// releasing the lock, so an in-place write here is a data race.
			t.stepWeightByName = weightsByName
		}
		p.mu.Unlock()

		// Seed PATCH carries full Content (template/step_rows/step_labels).
		// shapeSent is promoted below only after the seed lands, so a failed seed
		// doesn't leave pollActive ticks permanently skipping step_rows.
		if err := p.pw.UpdateActivity(ctx, slug, pushward.UpdateRequest{
			State: pushward.StateOngoing,
			Content: pushward.Content{
				Template:    pushward.TemplateSteps,
				Progress:    0.0,
				State:       "Starting...",
				Icon:        p.opts.Icon,
				Subtitle:    p.subtitle(repoShort, run.Name),
				AccentColor: pushward.ColorGreen,
				CurrentStep: pushward.IntPtr(0),
				TotalSteps:  pushward.IntPtr(initialTotalSteps),
				StepRows:    initialStepRows,
				StepLabels:  initialStepLabels,
				StepColors:  initialStepColors,
				StepWeights: initialStepWeights,
				// The slug is per-repo, so this content merge-patches over whatever
				// the last run left behind. A run superseded before its end frames
				// fired leaves live_progress on, which would put a countdown to the
				// old run's step in the header of a card that has not started a step
				// yet.
				LiveProgress: p.liveProgressOff(),
				// Both URLs are the forge's own, never composed here: a run's page
				// can be keyed off a display index that is not the id the API is
				// queried by.
				URL:          run.HTMLURL,
				SecondaryURL: run.RepoURL,
			},
		}); err != nil {
			p.log.Error("failed to send initial update", "slug", slug, "error", err)
			continue
		}

		p.mu.Lock()
		if t, ok := p.tracked[repo]; ok {
			t.shapeSent = initialTotalSteps
		}
		p.mu.Unlock()
	}
	return nil
}

// runAttrs builds the "workflow found" log fields, omitting the ones a forge
// does not report rather than logging them empty.
func (p *Poller) runAttrs(repo string, run Run, slug string) []any {
	attrs := []any{"repo", repo, "run_id", run.ID}
	if run.Number != 0 {
		attrs = append(attrs, "run", run.Number)
	}
	attrs = append(attrs, "workflow", run.Name, "branch", run.HeadBranch)
	if run.Event != "" {
		attrs = append(attrs, "event", run.Event)
	}
	return append(attrs, "slug", slug)
}

// subtitle is the card's second line, bounded so a forge with a long workflow
// name cannot push the payload past the server's rune cap and fail the send.
func (p *Poller) subtitle(repoShort, runName string) string {
	return text.TruncateHard(fmt.Sprintf("%s / %s", repoShort, runName), subtitleLimit)
}

// staleAfter is how long without a successful poll means the run vanished.
//
// It cannot key off StaleTimeout alone: that is a server-side TTL an operator may
// legally set to zero to take the server default, and a poll interval longer than
// it would then evict every healthy run one tick after it was created - a
// create/evict/re-create loop that never advances past the seed frame.
func (p *Poller) staleAfter() time.Duration {
	return max(p.opts.PushWard.StaleTimeout, p.opts.IdleInterval) + staleEvictionGrace
}

// shape computes the step shape and drops the opt-in pill fields the config
// disables. step_colors is omitempty, so a nil slice reproduces the payload a
// bridge sent before the field existed.
func (p *Poller) shape(jobs []ci.Job) ci.StepInfo {
	info := ci.ComputeSteps(jobs)
	if !p.opts.Render.StepColors {
		info.StepColors = nil
	}
	return info
}

// liveAnchor returns the unix window iOS animates the current step's pill
// across. The shared ladder decides whether there is anything worth animating;
// this only applies the config gate and supplies the ceiling that bounds a
// corrupt prior-run duration.
func (p *Poller) liveAnchor(info ci.StepInfo, byName map[string]float64, now time.Time) (start, end int64, ok bool) {
	if !p.opts.Render.LiveProgress {
		return 0, 0, false
	}
	return ci.LiveAnchor(info, byName, now, maxRunLifetime)
}

// liveProgressOff is the explicit false that stops an animation carried forward
// by merge-patch, or nil when the feature is disabled so the payload stays
// exactly what a bridge sent before the anchors existed.
func (p *Poller) liveProgressOff() *bool {
	if !p.opts.Render.LiveProgress {
		return nil
	}
	return pushward.BoolPtr(false)
}

// payloadWeights returns the step_weights slice to send, or nil when the
// duration-sized pills are switched off. The durations behind them are gathered
// whenever either consumer needs them, so this gates the wire field alone and
// leaves the live-progress anchors their measurements.
func (p *Poller) payloadWeights(labels []string, byName map[string]float64) []float64 {
	if !p.opts.Render.StepWeights {
		return nil
	}
	return ci.ProjectWeights(labels, byName)
}

// baselineShape returns the step shape of a prior run of the same workflow on
// the same branch, used to seed a stable total-steps denominator. A finished run
// has revealed its entire job DAG, so its group count is ground truth. Returns
// ok=false (so the caller keeps the current-run scan) when there is no usable
// prior run or any lookup fails.
//
// A blank branch is rejected: without it the lookup would seed from whatever
// branch ran most recently, whose job shape may differ. A blank WorkflowKey
// likewise can't target a workflow, so both short-circuit to the live scan
// before the forge is asked anything.
//
// The seed is an upper-or-lower estimate, not a guarantee. If this run takes a
// shorter path than the seed (if-gated jobs skipped), the total over-counts and
// the final frame shows the phantom steps as done (self-heals to N/N via
// scheduleEnd). If it grows past the seed, the pollActive clamp raises the total.
func (p *Poller) baselineShape(ctx context.Context, repo string, run Run) (ci.StepInfo, bool) {
	if run.WorkflowKey == "" || run.HeadBranch == "" {
		return ci.StepInfo{}, false
	}
	wantTimings := p.opts.Render.WantTimings()
	base, err := p.forge.BaselineJobs(ctx, repo, run, wantTimings)
	if err != nil {
		// Logged here rather than in each adapter: the decision this informs - keep
		// the live scan - is the loop's, and both forges would otherwise carry the
		// same two warnings.
		p.log.Warn("prior-run step seed unavailable, using the live scan",
			"repo", repo, "workflow", run.WorkflowKey, "branch", run.HeadBranch, "error", err)
		return ci.StepInfo{}, false
	}
	jobs := base.Jobs
	if len(jobs) == 0 {
		return ci.StepInfo{}, false
	}
	info := p.shape(jobs)
	// Measure how long each group ran in this finished run, keyed by group name
	// so the numbers attach to the right label even if the live run reveals its
	// groups in a different order. They size the pills and anchor the live
	// window, so collect them when either consumer is switched on.
	if wantTimings {
		info.WeightsByName = ci.GroupWeights(jobs)
	}
	p.log.Info("seeded steps from prior run",
		"repo", repo, "prev_run_id", base.RunID, "steps", info.TotalSteps,
		"step_rows", info.StepRows, "step_weights", info.WeightsByName)
	return info, true
}

func (p *Poller) pollActive(ctx context.Context) error {
	// Snapshot tracked keys under lock to avoid holding mutex across network calls
	p.mu.Lock()
	repos := make([]string, 0, len(p.tracked))
	for repo := range p.tracked {
		repos = append(repos, repo)
	}
	p.mu.Unlock()

	for _, repo := range repos {
		p.mu.Lock()
		t, ok := p.tracked[repo]
		if !ok || t.endTimers != nil {
			p.mu.Unlock()
			continue
		}
		// Eviction guard 1: the jobs endpoint stopped returning data for longer
		// than the server's stale TTL plus a grace period (the run vanished).
		// LastUpdate is refreshed on every successful poll, so this only fires for
		// runs that disappeared, NOT for runs stuck in progress.
		if !t.LastUpdate.IsZero() && time.Since(t.LastUpdate) > p.staleAfter() {
			delete(p.tracked, repo)
			p.log.Warn("evicted stale tracked run", "repo", repo, "run_id", t.RunID)
			p.mu.Unlock()
			continue
		}
		// Eviction guard 2: absolute age. A run wedged in progress (hung
		// self-hosted runner) keeps returning jobs, so LastUpdate never expires
		// and the bridge would track it - and block new-run detection for the
		// repo - forever. Reclaim it past a generous lifetime ceiling.
		if !t.trackedAt.IsZero() && time.Since(t.trackedAt) > maxRunLifetime {
			delete(p.tracked, repo)
			p.log.Warn("evicted run exceeding max lifetime", "repo", repo, "run_id", t.RunID, "age", time.Since(t.trackedAt).Round(time.Minute))
			p.mu.Unlock()
			continue
		}
		// Copy values needed for network calls
		tRunID := t.RunID
		tSlug := t.Slug
		tName := t.Name
		tHTMLURL := t.HTMLURL
		tRepoURL := t.RepoURL
		p.mu.Unlock()

		jobs, err := p.forge.LiveJobs(ctx, repo, tRunID)
		if err != nil {
			p.log.Error("getting jobs", "repo", repo, "error", err)
			continue
		}

		if len(jobs) == 0 {
			continue
		}

		info := p.shape(jobs)
		var weightsByName map[string]float64

		p.mu.Lock()
		if tt, ok := p.tracked[repo]; ok {
			tt.LastUpdate = time.Now()

			// Clamp TotalSteps to never decrease: forges lazily create jobs behind
			// needs/if conditions, so new steps appear over time. We keep the
			// highest total to avoid confusing jumps.
			if info.TotalSteps > tt.maxTotalSteps {
				p.log.Info("new steps discovered",
					"repo", repo, "jobs", len(jobs),
					"prev_steps", tt.maxTotalSteps, "new_steps", info.TotalSteps,
					"step_rows", info.StepRows)
				tt.maxTotalSteps = info.TotalSteps
				tt.maxStepRows = append([]int(nil), info.StepRows...)
				tt.maxStepLabels = append([]string(nil), info.StepLabels...)
				tt.maxStepColors = append([]string(nil), info.StepColors...)
			} else if info.TotalSteps < tt.maxTotalSteps {
				// Fewer groups than the seeded maximum: a wave the forge has not
				// revealed yet, or an if-gated job this run skipped. Keep the cached
				// shape so the denominator holds, and carry current_step across by
				// name first, while info.StepLabels still describes the list that
				// index was numbered against.
				info.CurrentStep = ci.RealignStep(info.CurrentStep, info.StepLabels, tt.maxStepLabels)
				info.TotalSteps = tt.maxTotalSteps
				info.StepRows = tt.maxStepRows
				info.StepLabels = tt.maxStepLabels
				info.StepColors = tt.maxStepColors
			}
			weightsByName = tt.stepWeightByName
		}
		p.mu.Unlock()

		// Size the pills from the prior run's durations, keyed by group name so
		// each weight tracks its label regardless of the order the forge reveals
		// the groups in (jobs can be added/reordered between runs). The result is
		// len(step_labels), so it never desyncs from total_steps; unknown groups
		// get the mean; no history yields nil (equal-width pills). Derived out here
		// rather than under p.mu: the map is immutable once published, and p.mu
		// serialises every repo, so the projection has no business holding it.
		stepWeights := p.payloadWeights(info.StepLabels, weightsByName)

		repoShort := repoName(repo)

		if info.AllCompleted {
			// All *visible* jobs are done, but forges create jobs lazily (reusable
			// workflows, if-gated jobs, dynamic matrices). Confirm the run itself
			// completed before ending - otherwise a poll landing between job waves
			// would prematurely dismiss the Live Activity.
			run, err := p.forge.GetRun(ctx, repo, tRunID)
			// A nil run alongside a nil error breaks the Forge contract, but mapping
			// a 404 to (nil, nil) is a common enough Go habit that treating it as a
			// deferral is worth more than the panic it would otherwise cause here.
			if err != nil || run == nil {
				p.log.Warn("failed to confirm run completion, deferring end", "repo", repo, "run_id", tRunID, "error", err)
				continue
			}
			if !run.Terminal() {
				// More jobs are still pending; keep the activity ongoing and let the
				// next wave surface on a subsequent poll.
				p.log.Debug("visible jobs complete but run still in progress, deferring end",
					"repo", repo, "run_id", tRunID, "status", run.RawStatus)
				continue
			}
			// The run's own outcome is authoritative; the ladder's AnyFailed only
			// covers a run that reports nothing usable of its own.
			state, color := p.forge.Outcome(*run, info.AnyFailed)
			p.log.Info("workflow completed",
				"repo", repo, "run_id", tRunID, "slug", tSlug,
				"status", run.RawStatus, "state", state)
			p.scheduleEnd(ctx, repo, pushward.Content{
				Template:    pushward.TemplateSteps,
				Progress:    1.0,
				State:       state,
				Icon:        p.opts.Icon,
				Subtitle:    p.subtitle(repoShort, tName),
				AccentColor: color,
				CurrentStep: pushward.IntPtr(info.TotalSteps),
				TotalSteps:  pushward.IntPtr(info.TotalSteps),
				StepRows:    info.StepRows,
				StepLabels:  info.StepLabels,
				StepColors:  info.StepColors,
				StepWeights: stepWeights,
				// Stop the animation on the result frames. Content updates are
				// merge-patches, so the last step's window survives otherwise, and
				// the server only strips the anchors from an END push: phase 1 is
				// ONGOING and would spend end_display_time counting toward a deadline
				// the run has already passed.
				LiveProgress: p.liveProgressOff(),
				URL:          tHTMLURL,
				SecondaryURL: tRepoURL,
			})
			continue
		}

		// Skip redundant ticks: a run parked on one long step yields identical
		// progress/state/steps across polls, and each PATCH pushes to every device.
		// Send only when a scalar changed, the forge revealed new jobs (shape grew),
		// or a heartbeat is due to keep the activity off the server's
		// stale-dismissal path.
		heartbeat := p.opts.PushWard.StaleTimeout / 2
		// Resolved before taking the lock: it reads only the scan, the immutable
		// published weights and the config.
		liveStart, liveEnd, wantLive := p.liveAnchor(info, weightsByName, time.Now())

		p.mu.Lock()
		tt, ok := p.tracked[repo]
		if !ok {
			p.mu.Unlock()
			continue
		}
		shapeChanged := tt.shapeSent < tt.maxTotalSteps
		scalarChanged := tt.lastPatchAt.IsZero() ||
			info.Progress != tt.lastProgress ||
			info.CurrentStepName != tt.lastState ||
			info.CurrentStep != tt.lastCurrentStep ||
			info.TotalSteps != tt.lastTotalSteps
		heartbeatDue := !tt.lastPatchAt.IsZero() && time.Since(tt.lastPatchAt) >= heartbeat
		// Anchor the live window on the group it belongs to, and only there:
		// restamping it on a later tick of the same group would snap the pill back
		// to empty. Dropping a stale anchor matters just as much: under merge-patch
		// an earlier live_progress:true would otherwise leave the new step animating
		// toward the old step's deadline.
		anchorChanged := wantLive && (!tt.liveSent || info.CurrentStepName != tt.liveStepName)
		// Only a step change can strand a window. A step that simply outruns its
		// estimate needs no push: iOS drops a window whose end has passed and falls
		// back to the static bar on its own, so switching live_progress off would
		// broadcast to every device to change nothing.
		clearLive := tt.liveSent && !wantLive && info.CurrentStepName != tt.liveStepName
		p.mu.Unlock()
		if !shapeChanged && !scalarChanged && !heartbeatDue && !anchorChanged && !clearLive {
			continue
		}

		// step_rows/step_labels are re-sent only when the forge lazily revealed new
		// jobs (totalSteps grew) - unchanged slices are preserved by the server
		// under merge-patch and re-sending them wastes payload bytes.
		contentPatch := &pushward.ContentPatch{
			Progress:    pushward.Float64Ptr(info.Progress),
			State:       pushward.StringPtr(info.CurrentStepName),
			CurrentStep: pushward.IntPtr(info.CurrentStep),
			TotalSteps:  pushward.IntPtr(info.TotalSteps),
		}
		if shapeChanged {
			contentPatch.StepRows = info.StepRows
			contentPatch.StepLabels = info.StepLabels
			contentPatch.StepColors = info.StepColors
			contentPatch.StepWeights = stepWeights
		}
		switch {
		case anchorChanged:
			contentPatch.LiveProgress = pushward.BoolPtr(true)
			contentPatch.StartDate = pushward.Int64Ptr(liveStart)
			contentPatch.EndDate = pushward.Int64Ptr(liveEnd)
			p.log.Debug("anchored live step window",
				"repo", repo, "step", info.CurrentStep, "name", info.CurrentStepName,
				"seconds", liveEnd-liveStart)
		case clearLive:
			contentPatch.LiveProgress = pushward.BoolPtr(false)
		}
		if err := p.pw.PatchActivity(ctx, tSlug, pushward.PatchRequest{
			State:   pushward.StateOngoing,
			Content: contentPatch,
		}); err != nil {
			p.log.Error("failed to update activity", "slug", tSlug, "error", err)
			continue
		}
		// Promote shape + scalar state only after a successful patch so a failed
		// send re-sends the shape and re-evaluates the scalars next tick.
		p.mu.Lock()
		if tt, ok := p.tracked[repo]; ok {
			if shapeChanged {
				tt.shapeSent = tt.maxTotalSteps
			}
			tt.lastProgress = info.Progress
			tt.lastState = info.CurrentStepName
			tt.lastCurrentStep = info.CurrentStep
			tt.lastTotalSteps = info.TotalSteps
			tt.lastPatchAt = time.Now()
			switch {
			case anchorChanged:
				tt.liveStepName = info.CurrentStepName
				tt.liveSent = true
			case clearLive:
				tt.liveStepName = ""
				tt.liveSent = false
			}
		}
		p.mu.Unlock()
	}
	return nil
}

// scheduleEnd schedules a two-phase end for an activity:
//   - Phase 1 (after EndDelay): ONGOING update with final content (visible in Dynamic Island)
//   - Phase 2 (EndDisplayTime later): ENDED with same content (dismisses Live Activity)
//
// This gives iOS time to register the push-update token after push-to-start, and
// lets the Dynamic Island show the final state before it is dismissed.
func (p *Poller) scheduleEnd(ctx context.Context, repo string, content pushward.Content) {
	p.mu.Lock()
	t, ok := p.tracked[repo]
	if !ok {
		p.mu.Unlock()
		return
	}
	slug := t.Slug
	runID := t.RunID
	endDelay := p.opts.PushWard.EndDelay
	displayTime := p.opts.PushWard.EndDisplayTime
	// Detach from the caller's context so delivery is not cut off mid-flight by
	// shutdown, while still inheriting any non-cancellation values (e.g. trace
	// IDs). Delivery is best-effort: on shutdown Run drains in-flight phases via
	// TimerGroup.Close + Wait, but a phase not yet fired is cancelled.
	detached := context.WithoutCancel(ctx)

	tg := &syncx.TimerGroup{}
	t.endTimers = tg
	p.mu.Unlock()

	tg.Reset(endDelay, func() {
		// Phase 1: ONGOING with final content
		ctx1, cancel1 := context.WithTimeout(detached, endPushTimeout)
		defer cancel1()
		ongoingReq := pushward.UpdateRequest{
			State:   pushward.StateOngoing,
			Content: content,
		}
		if err := p.pw.UpdateActivity(ctx1, slug, ongoingReq); err != nil {
			p.log.Error("failed to update activity (end phase 1)", "slug", slug, "error", err)
		} else {
			p.log.Info("updated activity", "slug", slug, "state", content.State)
		}

		// Phase 2: schedule ENDED after display time
		p.mu.Lock()
		current, ok := p.tracked[repo]
		p.mu.Unlock()
		if !ok || current.RunID != runID {
			return // cancelled between phases
		}
		tg.Reset(displayTime, func() {
			ctx2, cancel2 := context.WithTimeout(detached, endPushTimeout)
			defer cancel2()
			endedReq := pushward.UpdateRequest{
				State:   pushward.StateEnded,
				Content: content,
			}
			if err := p.pw.UpdateActivity(ctx2, slug, endedReq); err != nil {
				p.log.Error("failed to end activity (end phase 2)", "slug", slug, "error", err)
			} else {
				p.log.Info("ended activity", "slug", slug, "state", content.State)
			}

			// Server handles cleanup via ended_ttl - just remove from local map
			p.mu.Lock()
			if current, ok := p.tracked[repo]; ok && current.RunID == runID {
				delete(p.tracked, repo)
			}
			p.mu.Unlock()
		})
	})
}

// repoName is the short half of an "owner/name" repo path, which is what fits in
// the card's title and subtitle.
func repoName(fullRepo string) string {
	if _, name, ok := strings.Cut(fullRepo, "/"); ok {
		return name
	}
	return fullRepo
}
