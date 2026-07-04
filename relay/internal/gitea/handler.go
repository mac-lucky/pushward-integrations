package gitea

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/mac-lucky/pushward-integrations/relay/internal/auth"
	"github.com/mac-lucky/pushward-integrations/relay/internal/client"
	"github.com/mac-lucky/pushward-integrations/relay/internal/config"
	"github.com/mac-lucky/pushward-integrations/relay/internal/humautil"
	"github.com/mac-lucky/pushward-integrations/relay/internal/lifecycle"
	"github.com/mac-lucky/pushward-integrations/relay/internal/metrics"
	"github.com/mac-lucky/pushward-integrations/relay/internal/overrides"
	"github.com/mac-lucky/pushward-integrations/relay/internal/state"
	"github.com/mac-lucky/pushward-integrations/shared/pushward"
	"github.com/mac-lucky/pushward-integrations/shared/text"
)

// maxStepGroups bounds how many step groups the steps template carries before
// the per-group rows/labels are dropped to stay inside the APNs 4KB payload
// budget. Above the cap the steps template still renders the progress bar from
// current_step/total_steps without labels.
const maxStepGroups = 10

// stepIcon is the shared steps-template glyph across the run lifecycle.
const stepIcon = "arrow.triangle.branch"

type Handler struct {
	store   state.Store
	clients *client.Pool
	config  *config.GiteaConfig
	ender   *lifecycle.Ender
}

// RegisterRoutes registers the Gitea and Forgejo webhook endpoints and returns
// the Handler so the caller can collect the Ender for graceful shutdown.
//
// The Ender is built with a nil store: Gitea state uses a per-slug group schema
// (a "run" record plus "job:<id>" records) that the Ender's single-key Delete
// cannot clean. Group rows are bounded by StaleTimeout and cleared wholesale at
// supersede time, and the completed "run" tombstone is kept on purpose so late
// job events cannot resurrect an ended activity.
func RegisterRoutes(api huma.API, store state.Store, clients *client.Pool, cfg *config.GiteaConfig) *Handler {
	h := &Handler{
		store:   store,
		clients: clients,
		config:  cfg,
		ender: lifecycle.NewEnder(clients, nil, "gitea", lifecycle.EndConfig{
			EndDelay:       cfg.EndDelay,
			EndDisplayTime: cfg.EndDisplayTime,
		}),
	}
	humautil.RegisterWebhook(api, "/gitea", "post-gitea-webhook",
		"Receive Gitea Actions webhook",
		"Processes Gitea workflow_run and workflow_job events into a live build-progress Live Activity.",
		[]string{"Gitea"}, h.handleGitea)
	humautil.RegisterWebhook(api, "/forgejo", "post-forgejo-webhook",
		"Receive Forgejo Actions webhook",
		"Processes Forgejo action_run_* completion events into a Live Activity.",
		[]string{"Forgejo"}, h.handleForgejo)
	return h
}

func (h *Handler) Ender() *lifecycle.Ender { return h.ender }

// ---- Gitea (workflow_run / workflow_job) ----

func (h *Handler) handleGitea(ctx context.Context, input *struct {
	Body giteaPayload
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "gitea")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)
	p := &input.Body

	// Gitea is Live-Activity-only with no notification path, so
	// channels=notification has nothing to fall back to.
	if !overrides.FromContext(ctx).AllowsActivity() {
		return humautil.NewOK(), nil
	}

	if p.Repository == nil || p.Repository.FullName == "" {
		log.Debug("gitea event without repository, ignoring", "action", p.Action)
		return humautil.NewOK(), nil
	}

	var err error
	switch {
	case p.WorkflowJob != nil:
		err = h.handleJob(ctx, userKey, log, pwClient, p)
	case p.WorkflowRun != nil:
		err = h.handleRun(ctx, userKey, log, pwClient, p)
	default:
		// push, Test Delivery, and every non-Actions event fall through here.
		log.Debug("gitea event ignored", "action", p.Action)
	}
	if err != nil {
		return nil, huma.Error502BadGateway("upstream API error")
	}
	return humautil.NewOK(), nil
}

func (h *Handler) handleRun(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *giteaPayload) error {
	slug := giteaSlug(p.Repository.FullName)
	group := h.getGroup(ctx, log, userKey, slug)
	prevRun := parseRun(group)
	run := p.WorkflowRun

	switch p.Action {
	case "completed":
		return h.runCompleted(ctx, userKey, log, pwClient, p, slug, group, prevRun)
	case "requested", "in_progress":
		// Drop events for a run older than the one already tracked.
		if prevRun != nil && run.ID < prevRun.RunID {
			log.Debug("ignoring older gitea run", "slug", slug, "run_id", run.ID, "tracked", prevRun.RunID)
			return nil
		}
		// A late start for an already-completed run must not restart it.
		if prevRun != nil && prevRun.Completed && run.ID == prevRun.RunID {
			log.Debug("ignoring start for completed gitea run", "slug", slug, "run_id", run.ID)
			return nil
		}
		if prevRun == nil || run.ID > prevRun.RunID {
			// Brand-new run supersedes any prior one: cancel its pending end and
			// clear its job records so aggregation starts fresh.
			group = h.supersede(ctx, log, userKey, slug)
		}

		rec := runRecord{
			RunID:    run.ID,
			Workflow: h.workflowName(p),
			Branch:   run.HeadBranch,
			HTMLURL:  run.HTMLURL,
			RepoURL:  p.Repository.HTMLURL,
		}
		if err := h.putRun(ctx, userKey, slug, rec); err != nil {
			log.Error("failed to store run, continuing", "slug", slug, "error", err)
		}

		name := rec.Workflow
		if name == "" {
			name = repoShort(p.Repository.FullName)
		}
		if err := h.createActivity(ctx, pwClient, slug, name); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			return err
		}

		content := h.runningContent(p.Repository.FullName, &rec, parseJobs(group), p.Action)
		if err := pwClient.UpdateActivity(ctx, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
			log.Error("failed to update activity", "slug", slug, "error", err)
			return err
		}
		log.Info("gitea run started", "slug", slug, "run_id", run.ID, "action", p.Action)
		return nil
	default:
		log.Debug("gitea workflow_run action ignored", "action", p.Action)
		return nil
	}
}

func (h *Handler) runCompleted(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *giteaPayload, slug string, group map[string]json.RawMessage, prevRun *runRecord) error {
	run := p.WorkflowRun

	if prevRun != nil && run.ID < prevRun.RunID {
		log.Debug("ignoring completion for older gitea run", "slug", slug, "run_id", run.ID, "tracked", prevRun.RunID)
		return nil
	}
	if prevRun != nil && prevRun.Completed && run.ID == prevRun.RunID {
		log.Debug("duplicate gitea run completion ignored", "slug", slug, "run_id", run.ID)
		return nil
	}

	name := h.workflowName(p)
	created := prevRun != nil && run.ID == prevRun.RunID
	if !created {
		if prevRun != nil && run.ID > prevRun.RunID {
			group = h.supersede(ctx, log, userKey, slug)
		}
		if err := h.createActivity(ctx, pwClient, slug, name); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			return err
		}
	}

	rec := runRecord{
		RunID:     run.ID,
		Workflow:  name,
		Branch:    run.HeadBranch,
		HTMLURL:   run.HTMLURL,
		RepoURL:   p.Repository.HTMLURL,
		Completed: true,
	}
	if err := h.putRun(ctx, userKey, slug, rec); err != nil {
		log.Warn("failed to store completed run tombstone", "slug", slug, "error", err)
	}

	content := h.completedContent(p.Repository.FullName, &rec, run.Conclusion, parseJobs(group))
	h.ender.ScheduleEnd(userKey, slug, slug, content)
	log.Info("gitea run completed", "slug", slug, "run_id", run.ID, "conclusion", run.Conclusion)
	return nil
}

func (h *Handler) handleJob(ctx context.Context, userKey string, log *slog.Logger, pwClient *pushward.Client, p *giteaPayload) error {
	slug := giteaSlug(p.Repository.FullName)
	group := h.getGroup(ctx, log, userKey, slug)
	prevRun := parseRun(group)
	job := p.WorkflowJob

	if prevRun != nil {
		if job.RunID < prevRun.RunID {
			log.Debug("ignoring job from older gitea run", "slug", slug, "run_id", job.RunID, "tracked", prevRun.RunID)
			return nil
		}
		if job.RunID == prevRun.RunID && prevRun.Completed {
			log.Debug("ignoring job after gitea run completion", "slug", slug, "run_id", job.RunID)
			return nil
		}
		if job.RunID > prevRun.RunID {
			// A newer run whose workflow_run has not arrived yet: supersede and
			// lazily start from this job.
			group = h.supersede(ctx, log, userKey, slug)
			prevRun = nil
		}
	}

	if prevRun == nil {
		// Lazy-create: no run record yet (job-only subscription, or a job event
		// racing ahead of its workflow_run). Fill what the job event carries.
		rec := runRecord{RunID: job.RunID, HTMLURL: job.HTMLURL, RepoURL: p.Repository.HTMLURL}
		if err := h.putRun(ctx, userKey, slug, rec); err != nil {
			log.Error("failed to store run, continuing", "slug", slug, "error", err)
		}
		if err := h.createActivity(ctx, pwClient, slug, repoShort(p.Repository.FullName)); err != nil {
			log.Error("failed to create activity", "slug", slug, "error", err)
			return err
		}
		prevRun = &rec
		group = nil
	}

	jr := jobRecord{ID: job.ID, Name: job.Name, Status: job.Status, Conclusion: job.Conclusion}
	if err := h.putJob(ctx, userKey, slug, jr); err != nil {
		log.Error("failed to store job, continuing", "slug", slug, "error", err)
	}
	jobs := upsertJob(parseJobs(group), jr)

	info := computeSteps(jobs)
	state := info.CurrentStepName
	if state == "" {
		state = "Running"
	}
	runURL := prevRun.HTMLURL
	if runURL == "" {
		runURL = job.HTMLURL
	}
	content := pushward.Content{
		Template:     pushward.TemplateSteps,
		Progress:     info.Progress,
		State:        state,
		Icon:         stepIcon,
		Subtitle:     subtitle("Gitea", p.Repository.FullName, prevRun.Workflow),
		AccentColor:  pushward.ColorBlue,
		URL:          text.SanitizeURL(runURL),
		SecondaryURL: text.SanitizeURL(p.Repository.HTMLURL),
		CurrentStep:  pushward.IntPtr(info.CurrentStep),
		TotalSteps:   pushward.IntPtr(info.TotalSteps),
	}
	content.StepRows, content.StepLabels = stepRowsLabels(info)

	if err := pwClient.UpdateActivity(ctx, slug, pushward.UpdateRequest{State: pushward.StateOngoing, Content: content}); err != nil {
		log.Error("failed to update activity", "slug", slug, "error", err)
		return err
	}
	log.Info("gitea job update", "slug", slug, "run_id", job.RunID, "job", job.Name, "status", job.Status)
	return nil
}

// runningContent builds the ONGOING steps content for a run that is queued or in
// progress. With no jobs yet it shows a single placeholder step; otherwise it
// aggregates the visible jobs.
func (h *Handler) runningContent(repoFull string, rec *runRecord, jobs []jobRecord, action string) pushward.Content {
	content := pushward.Content{
		Template:     pushward.TemplateSteps,
		Icon:         stepIcon,
		AccentColor:  pushward.ColorBlue,
		Subtitle:     subtitle("Gitea", repoFull, rec.Workflow),
		URL:          text.SanitizeURL(rec.HTMLURL),
		SecondaryURL: text.SanitizeURL(rec.RepoURL),
	}
	if len(jobs) == 0 {
		content.State = "Queued"
		if action == "in_progress" {
			content.State = "Running"
		}
		content.CurrentStep = pushward.IntPtr(0)
		content.TotalSteps = pushward.IntPtr(1)
		return content
	}
	info := computeSteps(jobs)
	state := info.CurrentStepName
	if state == "" {
		state = "Running"
	}
	content.State = state
	content.Progress = info.Progress
	content.CurrentStep = pushward.IntPtr(info.CurrentStep)
	content.TotalSteps = pushward.IntPtr(info.TotalSteps)
	content.StepRows, content.StepLabels = stepRowsLabels(info)
	return content
}

// completedContent builds the final steps frame for a finished run.
func (h *Handler) completedContent(repoFull string, rec *runRecord, conclusion string, jobs []jobRecord) pushward.Content {
	state, color := conclusionState(conclusion)
	if conclusion == "" && len(jobs) > 0 && computeSteps(jobs).AnyFailed {
		state, color = "Failed", pushward.ColorRed
	}
	content := pushward.Content{
		Template:     pushward.TemplateSteps,
		Progress:     1.0,
		State:        state,
		Icon:         stepIcon,
		Subtitle:     subtitle("Gitea", repoFull, rec.Workflow),
		AccentColor:  color,
		URL:          text.SanitizeURL(rec.HTMLURL),
		SecondaryURL: text.SanitizeURL(rec.RepoURL),
	}
	if len(jobs) > 0 {
		info := computeSteps(jobs)
		content.CurrentStep = pushward.IntPtr(info.TotalSteps)
		content.TotalSteps = pushward.IntPtr(info.TotalSteps)
		content.StepRows, content.StepLabels = stepRowsLabels(info)
	} else {
		content.CurrentStep = pushward.IntPtr(1)
		content.TotalSteps = pushward.IntPtr(1)
	}
	return content
}

// ---- Forgejo (action_run_* completion events) ----

func (h *Handler) handleForgejo(ctx context.Context, input *struct {
	Body forgejoPayload
},
) (*humautil.WebhookResponse, error) {
	ctx = metrics.WithProvider(ctx, "forgejo")
	userKey := auth.KeyFromContext(ctx)
	log := slog.With("tenant", auth.KeyHash(userKey))
	pwClient := h.clients.Get(userKey)
	p := &input.Body

	if !overrides.FromContext(ctx).AllowsActivity() {
		return humautil.NewOK(), nil
	}

	if p.Run == nil || p.Run.Repository == nil || p.Run.Repository.FullName == "" {
		log.Debug("forgejo event without run/repository, ignoring", "action", p.Action)
		return humautil.NewOK(), nil
	}

	var stateText, color, icon string
	switch p.Action {
	case "success":
		stateText, color, icon = "Succeeded", pushward.ColorGreen, "checkmark.circle.fill"
	case "recover":
		stateText, color, icon = "Recovered", pushward.ColorGreen, "checkmark.circle.fill"
	case "failure":
		stateText, color, icon = "Failed", pushward.ColorRed, "xmark.circle.fill"
	default:
		log.Debug("forgejo action ignored", "action", p.Action)
		return humautil.NewOK(), nil
	}

	run := p.Run
	slug := forgejoSlug(run.Repository.FullName)
	name := run.Title
	if name == "" {
		name = run.WorkflowID
	}
	if name == "" {
		name = repoShort(run.Repository.FullName)
	}
	if err := h.createActivity(ctx, pwClient, slug, name); err != nil {
		log.Error("failed to create forgejo activity", "slug", slug, "error", err)
		return nil, huma.Error502BadGateway("upstream API error")
	}

	content := pushward.Content{
		Template:    pushward.TemplateGeneric,
		Progress:    1.0,
		State:       stateText,
		Icon:        icon,
		Subtitle:    subtitle("Forgejo", run.Repository.FullName, run.WorkflowID),
		AccentColor: color,
		URL:         text.SanitizeURL(run.HTMLURL),
	}
	// success and recover can both fire for one run; ScheduleEnd supersedes the
	// prior timer so the last write wins (harmless).
	h.ender.ScheduleEnd(userKey, slug, slug, content)
	log.Info("forgejo run completed", "slug", slug, "action", p.Action)
	return humautil.NewOK(), nil
}

// ---- shared helpers ----

func (h *Handler) createActivity(ctx context.Context, pwClient *pushward.Client, slug, name string) error {
	endedTTL := int(h.config.CleanupDelay.Seconds())
	staleTTL := int(h.config.StaleTimeout.Seconds())
	return pwClient.CreateActivity(ctx, slug, text.TruncateHard(name, 100), overrides.FromContext(ctx).PriorityOr(h.config.Priority), endedTTL, staleTTL)
}

// getGroup reads the per-slug state group, degrading to an empty group on a
// store error so a transient DB blip does not drop a webhook.
func (h *Handler) getGroup(ctx context.Context, log *slog.Logger, userKey, slug string) map[string]json.RawMessage {
	group, err := h.store.GetGroup(ctx, "gitea", userKey, slug)
	if err != nil {
		log.Error("failed to read state, continuing", "slug", slug, "error", err)
		return nil
	}
	return group
}

// supersede cancels a pending end for the slug and clears its state group so a
// newer run starts from a clean slate. Returns the now-empty group.
func (h *Handler) supersede(ctx context.Context, log *slog.Logger, userKey, slug string) map[string]json.RawMessage {
	h.ender.StopTimer(userKey, slug)
	if err := h.store.DeleteGroup(ctx, "gitea", userKey, slug); err != nil {
		log.Warn("state group delete failed", "error", err, "slug", slug)
	}
	return nil
}

func (h *Handler) putRun(ctx context.Context, userKey, slug string, rec runRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return h.store.Set(ctx, "gitea", userKey, slug, "run", data, h.config.StaleTimeout)
}

func (h *Handler) putJob(ctx context.Context, userKey, slug string, jr jobRecord) error {
	data, err := json.Marshal(jr)
	if err != nil {
		return err
	}
	return h.store.Set(ctx, "gitea", userKey, slug, "job:"+strconv.FormatInt(jr.ID, 10), data, h.config.StaleTimeout)
}

// workflowName resolves the human name of a run. Gitea does not populate a name
// on workflow_run, so it comes from the workflow object, falling back to the
// run's display title and finally the repository short name.
func (h *Handler) workflowName(p *giteaPayload) string {
	if p.Workflow != nil && p.Workflow.Name != "" {
		return p.Workflow.Name
	}
	if p.WorkflowRun != nil && p.WorkflowRun.DisplayTitle != "" {
		return p.WorkflowRun.DisplayTitle
	}
	return repoShort(p.Repository.FullName)
}

func giteaSlug(fullName string) string   { return text.SlugHash("gitea", fullName, 4) }
func forgejoSlug(fullName string) string { return text.SlugHash("forgejo", fullName, 4) }

func repoShort(fullName string) string {
	if i := strings.LastIndex(fullName, "/"); i != -1 {
		return fullName[i+1:]
	}
	return fullName
}

// subtitle renders the "<Brand> \u00b7 <repo> / <workflow>" activity subtitle,
// bounded to keep the whole string comfortably under the server's 256-rune cap.
func subtitle(brand, repoFull, workflow string) string {
	label := repoShort(repoFull)
	if workflow != "" {
		label = label + " / " + workflow
	}
	return brand + " \u00b7 " + text.TruncateHard(label, 60)
}

// conclusionState maps a Gitea run conclusion to a display state and color.
func conclusionState(conclusion string) (string, string) {
	switch conclusion {
	case "success":
		return "Success", pushward.ColorGreen
	case "failure":
		return "Failed", pushward.ColorRed
	case "cancelled":
		return "Cancelled", pushward.ColorOrange
	case "skipped":
		return "Skipped", pushward.ColorBlue
	case "":
		return "Complete", pushward.ColorGreen
	default:
		return titleCase(conclusion), pushward.ColorOrange
	}
}

// stepRowsLabels returns the clamped step rows and truncated labels for the
// steps template, or nil slices when the group count exceeds the 4KB payload
// cap (the template renders without labels in that case). Row counts are
// clamped to 1-10 to satisfy the server's per-group job-count bound.
func stepRowsLabels(info stepInfo) ([]int, []string) {
	if info.TotalSteps < 1 || info.TotalSteps > maxStepGroups {
		return nil, nil
	}
	rows := make([]int, len(info.StepRows))
	labels := make([]string, len(info.StepLabels))
	for i := range info.StepRows {
		v := info.StepRows[i]
		if v < 1 {
			v = 1
		} else if v > 10 {
			v = 10
		}
		rows[i] = v
		labels[i] = text.TruncateHard(info.StepLabels[i], 32)
	}
	return rows, labels
}

func parseRun(group map[string]json.RawMessage) *runRecord {
	raw, ok := group["run"]
	if !ok {
		return nil
	}
	var rec runRecord
	if json.Unmarshal(raw, &rec) != nil {
		return nil
	}
	return &rec
}

func parseJobs(group map[string]json.RawMessage) []jobRecord {
	var jobs []jobRecord
	for k, raw := range group {
		if !strings.HasPrefix(k, "job:") {
			continue
		}
		var jr jobRecord
		if json.Unmarshal(raw, &jr) == nil {
			jobs = append(jobs, jr)
		}
	}
	sortJobs(jobs)
	return jobs
}

func upsertJob(jobs []jobRecord, jr jobRecord) []jobRecord {
	for i := range jobs {
		if jobs[i].ID == jr.ID {
			jobs[i] = jr
			return jobs
		}
	}
	jobs = append(jobs, jr)
	sortJobs(jobs)
	return jobs
}

// sortJobs orders jobs by ID so step groups stay in a stable, roughly
// creation-ordered sequence across updates.
func sortJobs(jobs []jobRecord) {
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
}

// titleCase capitalises the first rune of s.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}
