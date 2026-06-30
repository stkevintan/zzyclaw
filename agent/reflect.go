package agent

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strings"
	"sync"
	"time"

	"zzy/copilot"
)

// reflectItem mirrors one extracted memory point in the model's JSON output.
type reflectItem struct {
	Index  string `json:"index"`
	Detail string `json:"detail"`
}

// reflectionResult is the structured output of one reflection pass.
type reflectionResult struct {
	Personal  []reflectItem `json:"personal"`
	Feedback  []reflectItem `json:"feedback"`
	Project   []reflectItem `json:"project"`
	Reference []reflectItem `json:"reference"`
}

const reflectionSystemPrompt = `You distill durable memory from a chat conversation into four categories:
- personal: the user's character, stable preferences, role, and working style.
- feedback: explicit feedback, choices, corrections, and decisions the user made.
- project: facts about the current project (stack, goals, constraints, paths, names).
- reference: other durable, reusable facts worth recalling later.
For each point output {"index","detail"}: index is a short, self-contained one-line summary;
detail is the fuller context. Capture only stable, reusable points — skip transient, task-specific chatter.
You are given the existing memory; do NOT repeat points already covered. Emit only new or changed points.
Keep it concise: prefer few high-value points over many. Respond as JSON with keys
personal, feedback, project, reference, each an array (possibly empty). Use the conversation's language.`

const consolidateSystemPrompt = `You merge an overgrown list of memory notes into fewer, denser ones without losing specifics.
Combine overlapping notes, drop redundancy, keep names, ids, paths and numbers. For each note output
{"index","detail"}: a short one-line index and a fuller detail. Respond as JSON {"items":[...]}.`

type consolidateResult struct {
	Items []reflectItem `json:"items"`
}

// ReflectorConfig tunes idle reflection.
type ReflectorConfig struct {
	// IdleDelay is the quiet period after a turn before reflection runs.
	IdleDelay time.Duration
	// MinMessages skips reflection for conversations shorter than this.
	MinMessages int
	// SoftCap is the per-category count above which a consolidation pass runs.
	SoftCap int
}

// Reflector watches sessions and, after they go idle, distills a read-only
// snapshot of the conversation into structural memory. It never holds a live
// session's lock: callers snapshot the history themselves, and the model call
// runs on a background goroutine scoped to baseCtx.
type Reflector struct {
	store   Store
	mem     StructuralMemory
	client  *copilot.Client
	idle    time.Duration
	minMsgs int
	softCap int
	perCat  int
	baseCtx context.Context

	mu       sync.Mutex
	jobs     map[string]*reflectJob
	inflight map[string]bool

	// extract is a field so tests can inject a deterministic extractor.
	extract func(ctx context.Context, transcript, existing string) (reflectionResult, error)
}

type reflectJob struct {
	timer    *time.Timer
	userID   string
	snapshot []copilot.Message
}

// NewReflector returns a reflector bound to store/mem/client. baseCtx scopes all
// background work; cancel it (or call Stop) to halt pending reflections.
func NewReflector(baseCtx context.Context, store Store, mem StructuralMemory, client *copilot.Client, cfg ReflectorConfig) *Reflector {
	if cfg.IdleDelay <= 0 {
		cfg.IdleDelay = 120 * time.Second
	}
	if cfg.MinMessages <= 0 {
		cfg.MinMessages = 4
	}
	if cfg.SoftCap <= 0 {
		cfg.SoftCap = 30
	}
	r := &Reflector{
		store:    store,
		mem:      mem,
		client:   client,
		idle:     cfg.IdleDelay,
		minMsgs:  cfg.MinMessages,
		softCap:  cfg.SoftCap,
		perCat:   structDefaultPerCat,
		baseCtx:  baseCtx,
		jobs:     make(map[string]*reflectJob),
		inflight: make(map[string]bool),
	}
	r.extract = r.extractWithModel
	return r
}

// Schedule (re)arms idle reflection for a session with the latest snapshot.
// It debounces: each new turn resets the per-session timer, so reflection fires
// only once the conversation has been quiet for the idle delay. The first call
// arms a time.AfterFunc; later calls reuse and Reset the same timer.
func (r *Reflector) Schedule(userID, sessionKey string, history []copilot.Message) {
	if r == nil || sessionKey == "" {
		return
	}
	snap := append([]copilot.Message(nil), history...)
	r.mu.Lock()
	defer r.mu.Unlock()
	if job, ok := r.jobs[sessionKey]; ok {
		job.snapshot = snap
		job.userID = userID
		job.timer.Reset(r.idle)
		return
	}
	job := &reflectJob{userID: userID, snapshot: snap}
	job.timer = time.AfterFunc(r.idle, func() { r.fire(sessionKey) })
	r.jobs[sessionKey] = job
}

// Stop cancels all pending (not-yet-fired) timers. Reflections already running
// are not interrupted here; cancel baseCtx to unwind those.
func (r *Reflector) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, job := range r.jobs {
		job.timer.Stop()
		delete(r.jobs, k)
	}
}

// fire runs when a session's debounce timer elapses. It claims the job (moving
// it from jobs to inflight so a concurrent Schedule can't double-run it) and
// then reflects outside the lock.
func (r *Reflector) fire(sessionKey string) {
	r.mu.Lock()
	job, ok := r.jobs[sessionKey]
	if !ok {
		r.mu.Unlock()
		return
	}
	if r.inflight[sessionKey] {
		// A previous reflection for this session is still running. Don't drop the
		// job (that would orphan it in r.jobs with a spent timer and starve future
		// reflections); re-arm the timer to retry once the in-flight pass finishes.
		job.timer.Reset(r.idle)
		r.mu.Unlock()
		return
	}
	delete(r.jobs, sessionKey)
	r.inflight[sessionKey] = true
	userID, snap := job.userID, job.snapshot
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.inflight, sessionKey)
		r.mu.Unlock()
	}()

	if err := r.reflect(r.baseCtx, userID, sessionKey, snap); err != nil {
		slog.WarnContext(r.baseCtx, "memory reflection failed", "user_id", userID, "error", err)
	}
}

func wmKey(sessionKey string) string { return "structmem:wm:" + sessionKey }

func (r *Reflector) reflect(ctx context.Context, userID, sessionKey string, snap []copilot.Message) error {
	if userID == "" || len(snap) < r.minMsgs {
		return nil
	}
	transcript := renderTranscript(snap)
	if strings.TrimSpace(transcript) == "" {
		return nil
	}
	// Hash-based watermark: skip when the conversation is unchanged since the last
	// reflection. Robust to history trimming/compaction (which shifts indexes).
	sum := hashTranscript(transcript)
	if prev, err := r.store.GetMeta(ctx, wmKey(sessionKey)); err == nil && string(prev) == sum {
		return nil
	}

	existing := r.existingSummary(ctx, userID)
	res, err := r.extract(ctx, transcript, existing)
	if err != nil {
		return err
	}
	for _, b := range []struct {
		cat   MemoryCategory
		items []reflectItem
	}{
		{CategoryPersonal, res.Personal},
		{CategoryFeedback, res.Feedback},
		{CategoryProject, res.Project},
		{CategoryReference, res.Reference},
	} {
		for _, it := range b.items {
			if strings.TrimSpace(it.Index) == "" {
				continue
			}
			if _, err := r.mem.Upsert(ctx, userID, b.cat, it.Index, it.Detail); err != nil {
				slog.WarnContext(ctx, "memory upsert failed", "category", b.cat, "error", err)
			}
		}
		r.consolidate(ctx, userID, b.cat)
	}
	_ = r.store.SetMeta(ctx, wmKey(sessionKey), []byte(sum))
	return nil
}

// existingSummary lists current indexes so the extractor avoids duplicating them.
func (r *Reflector) existingSummary(ctx context.Context, userID string) string {
	entries, err := r.mem.List(ctx, userID)
	if err != nil || len(entries) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s] %s\n", e.Category, e.Index)
	}
	return strings.TrimRight(b.String(), "\n")
}

// consolidate merges a category once it grows past the soft cap.
func (r *Reflector) consolidate(ctx context.Context, userID string, cat MemoryCategory) {
	entries, err := r.mem.List(ctx, userID)
	if err != nil {
		return
	}
	var inCat []MemoEntry
	for _, e := range entries {
		if e.Category == cat {
			inCat = append(inCat, e)
		}
	}
	if len(inCat) <= r.softCap {
		return
	}
	var b strings.Builder
	for _, e := range inCat {
		fmt.Fprintf(&b, "- %s :: %s\n", e.Index, e.Detail)
	}
	out, err := copilot.Parse[consolidateResult](ctx, r.client, consolidateSystemPrompt, "Notes to merge:\n"+b.String())
	if err != nil || out == nil || len(out.Items) == 0 {
		return
	}
	drafts := make([]MemoDraft, 0, len(out.Items))
	for _, it := range out.Items {
		drafts = append(drafts, MemoDraft(it))
	}
	if err := r.mem.ReplaceCategory(ctx, userID, cat, drafts); err != nil {
		slog.WarnContext(ctx, "memory consolidation failed", "category", cat, "error", err)
	}
}

func (r *Reflector) extractWithModel(ctx context.Context, transcript, existing string) (reflectionResult, error) {
	content := "Existing memory (do not duplicate):\n" + existing + "\n\nConversation:\n" + transcript
	out, err := copilot.Parse[reflectionResult](ctx, r.client, reflectionSystemPrompt, content)
	if err != nil {
		return reflectionResult{}, err
	}
	if out == nil {
		return reflectionResult{}, fmt.Errorf("reflector: parsed reflection result is nil")
	}
	return *out, nil
}

func hashTranscript(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return fmt.Sprintf("%x", h.Sum64())
}
