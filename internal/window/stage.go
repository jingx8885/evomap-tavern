package window

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	KindImage  = "image"
	KindVideo  = "video"
	KindSpeech = "speech"
	KindSong   = "song"
	KindLLM    = "llm"
	// KindCodex is a Codex CLI run in its own scratch directory.
	KindCodex = "codex"

	StatusQueued   = "queued"
	StatusRunning  = "running"
	StatusReady    = "ready"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"

	LayoutQueue = "queue"
	LayoutStage = "stage"
	LayoutSplit = "split"

	OpLayoutQueue   = "layout_queue"
	OpLayoutStage   = "layout_stage"
	OpLayoutSplit   = "layout_split"
	OpFeature       = "feature"
	OpCancel        = "cancel"
	OpDecorate      = "decorate"
	OpAddControl    = "add_control"
	OpClearControls = "clear_controls"

	TargetLatest = "latest"
	TargetNone   = "none"
)

// Job is one queued picture, clip, or note.
type Job struct {
	ID     string  `json:"id"`
	Kind   string  `json:"kind"`
	Prompt string  `json:"prompt"`
	Status string  `json:"status"`
	Ratio  float64 `json:"ratio"`
	Detail string  `json:"detail,omitempty"`
	File   string  `json:"file,omitempty"`
	Text   string  `json:"text,omitempty"`
	Look   string  `json:"look,omitempty"`
	Err    string  `json:"err,omitempty"`
	// Unix milliseconds; zero until the job reaches that point.
	Created int64 `json:"created,omitempty"`
	Started int64 `json:"started,omitempty"`
	Ended   int64 `json:"ended,omitempty"`
}

func nowMS() int64 { return time.Now().UnixMilli() }

// Runner performs one job. report may be called as it advances.
// file is a base name under the media dir; text is a note.
type Runner func(ctx context.Context, kind, prompt string, report func(status string, ratio float64)) (file, text string, err error)

// Stage is the queue page: pictures, clips, and llm notes.
// States follow the ComfyUI split of a pending queue and a finished
// result (https://github.com/comfyanonymous/ComfyUI): queued, running,
// then ready or failed. The page and Glance describe the same regions.
type Stage struct {
	mu       sync.Mutex
	seq      int
	jobs     []Job
	layout   string
	feature  string
	theme    Theme
	controls []Control
	runner   Runner
	wake     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
	running  map[string]context.CancelFunc
	waiters  map[string][]chan Job
	changed  func()
	closed   bool
}

// StageOptions configures the queue module.
type StageOptions struct {
	Runner  Runner
	Changed func()
}

// NewStage starts a worker immediately. Close stops it.
func NewStage(opt StageOptions) *Stage {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Stage{
		layout:  LayoutSplit,
		theme:   DefaultTheme(),
		runner:  opt.Runner,
		changed: opt.Changed,
		wake:    make(chan struct{}, 1),
		ctx:     ctx,
		cancel:  cancel,
		running: map[string]context.CancelFunc{},
		waiters: map[string][]chan Job{},
	}
	go s.loop()
	return s
}

func (s *Stage) ID() string    { return "stage" }
func (s *Stage) Title() string { return "stage" }

func (s *Stage) Ops() map[string]string {
	ops := map[string]string{
		OpLayoutQueue:   "Left column of job cards only.",
		OpLayoutStage:   "Right frame only, the featured job.",
		OpLayoutSplit:   "Left column of cards and a right frame.",
		OpDecorate:      "Restyle colors, type, corners, and frame from what they just asked. A slow model writes the theme; code checks it before it is shown.",
		OpAddControl:    "Add a few checked controls (label, note, chip, rule, meter) from what they just asked. A slow model writes them; code rejects anything else.",
		OpClearControls: "Remove the controls she added. The queue and the theme stay.",
	}
	if s == nil {
		return ops
	}
	s.mu.Lock()
	n := len(s.jobs)
	s.mu.Unlock()
	if n > 0 {
		ops[OpFeature] = "Put win_target in the right frame."
		ops[OpCancel] = "Stop the job named by win_target."
	}
	return ops
}

func (s *Stage) Targets() []Target {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.jobs) == 0 {
		return nil
	}
	out := []Target{
		{ID: TargetNone, Label: "Do not change which job is featured."},
		{ID: TargetLatest, Label: "The newest job."},
	}
	start := 0
	if len(s.jobs) > 6 {
		start = len(s.jobs) - 6
	}
	for _, j := range s.jobs[start:] {
		out = append(out, Target{ID: j.ID, Label: j.Kind + " " + j.Status + " " + clip(j.Prompt, 48)})
	}
	return out
}

// Apply changes layout, feature, or cancels a job. Unknown ops are refused.
func (s *Stage) Apply(op, target string) error {
	if s == nil {
		return fmt.Errorf("stage required")
	}
	s.mu.Lock()
	err := s.applyLocked(op, target)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.ping()
	return nil
}

func (s *Stage) applyLocked(op, target string) error {
	switch op {
	case OpLayoutQueue:
		s.layout = LayoutQueue
	case OpLayoutStage:
		s.layout = LayoutStage
	case OpLayoutSplit:
		s.layout = LayoutSplit
	case OpFeature:
		id := s.resolveLocked(target)
		if id == "" {
			return fmt.Errorf("no job to feature")
		}
		s.feature = id
	case OpCancel:
		id := s.resolveLocked(target)
		if id == "" {
			return fmt.Errorf("no job to cancel")
		}
		s.cancelLocked(id)
	case OpClearControls:
		s.controls = nil
	case OpDecorate, OpAddControl:
		return fmt.Errorf("%s is applied from a checked model draft", op)
	default:
		return fmt.Errorf("unknown stage op %q", op)
	}
	return nil
}

func (s *Stage) resolveLocked(target string) string {
	target = strings.TrimSpace(target)
	if target == "" || target == TargetLatest {
		return s.latestLocked()
	}
	if target == TargetNone {
		return ""
	}
	for i := range s.jobs {
		if s.jobs[i].ID == target {
			return target
		}
	}
	return ""
}

func (s *Stage) latestLocked() string {
	for i := len(s.jobs) - 1; i >= 0; i-- {
		if s.jobs[i].Status != StatusCanceled {
			return s.jobs[i].ID
		}
	}
	if len(s.jobs) == 0 {
		return ""
	}
	return s.jobs[len(s.jobs)-1].ID
}

func (s *Stage) cancelLocked(id string) {
	for i := range s.jobs {
		if s.jobs[i].ID != id {
			continue
		}
		switch s.jobs[i].Status {
		case StatusReady, StatusFailed, StatusCanceled:
			return
		case StatusQueued:
			s.jobs[i].Status = StatusCanceled
			s.jobs[i].Detail = "canceled"
			s.jobs[i].Ended = nowMS()
			s.deliverLocked(s.jobs[i])
		default:
			if cancel := s.running[id]; cancel != nil {
				cancel()
			}
		}
		return
	}
}

// Enqueue adds a job and wakes the worker. The prompt is already concrete.
func (s *Stage) Enqueue(kind, prompt string) Job {
	kind = strings.TrimSpace(kind)
	prompt = strings.TrimSpace(prompt)
	s.mu.Lock()
	s.seq++
	job := Job{
		ID:     fmt.Sprintf("j%d", s.seq),
		Kind:   kind,
		Prompt: prompt,
		Status:  StatusQueued,
		Detail:  "queued",
		Created: nowMS(),
	}
	if prompt == "" || !knownKind(kind) {
		job.Status = StatusFailed
		job.Err = "bad job"
		job.Detail = "failed"
		job.Ended = job.Created
	}
	s.jobs = append(s.jobs, job)
	s.trimLocked()
	if s.feature == "" {
		s.feature = job.ID
	}
	failed := job.Status == StatusFailed
	if failed {
		s.deliverLocked(job)
	} else {
		s.kick()
	}
	s.mu.Unlock()
	s.ping()
	return job
}

func knownKind(kind string) bool {
	switch kind {
	case KindImage, KindVideo, KindSpeech, KindSong, KindLLM, KindCodex:
		return true
	default:
		return false
	}
}

func (s *Stage) trimLocked() {
	const maxJobs = 16
	if len(s.jobs) <= maxJobs {
		return
	}
	drop := len(s.jobs) - maxJobs
	s.jobs = append([]Job(nil), s.jobs[drop:]...)
}

// Wait blocks until id reaches a terminal status or ctx ends.
func (s *Stage) Wait(ctx context.Context, id string) (Job, error) {
	if s == nil {
		return Job{}, fmt.Errorf("stage required")
	}
	s.mu.Lock()
	for _, j := range s.jobs {
		if j.ID == id && terminal(j.Status) {
			s.mu.Unlock()
			return j, nil
		}
	}
	ch := make(chan Job, 1)
	s.waiters[id] = append(s.waiters[id], ch)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return Job{}, ctx.Err()
	case job := <-ch:
		return job, nil
	}
}

// SetLook attaches a caption to a finished picture and refreshes the glance.
func (s *Stage) SetLook(id, look string) {
	if s == nil {
		return
	}
	look = strings.TrimSpace(look)
	s.mu.Lock()
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			s.jobs[i].Look = look
			break
		}
	}
	s.mu.Unlock()
	s.ping()
}

// Layout is queue, stage, or split.
func (s *Stage) Layout() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.layout
}

// Snapshot copies the jobs, newest last.
func (s *Stage) Snapshot() []Job {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Job(nil), s.jobs...)
}

func (s *Stage) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancel()
	for _, cancel := range s.running {
		cancel()
	}
	s.mu.Unlock()
}

func (s *Stage) loop() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		}
		for {
			job, runCtx, ok := s.claim()
			if !ok {
				break
			}
			s.ping()
			s.execute(runCtx, job)
		}
	}
}

func (s *Stage) claim() (Job, context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Job{}, nil, false
	}
	for i := range s.jobs {
		if s.jobs[i].Status != StatusQueued {
			continue
		}
		s.jobs[i].Status = StatusRunning
		s.jobs[i].Detail = "running"
		s.jobs[i].Ratio = 0.05
		s.jobs[i].Started = nowMS()
		ctx, cancel := context.WithCancel(s.ctx)
		s.running[s.jobs[i].ID] = cancel
		job := s.jobs[i]
		return job, ctx, true
	}
	return Job{}, nil, false
}

func (s *Stage) execute(ctx context.Context, job Job) {
	report := func(status string, ratio float64) {
		s.mu.Lock()
		s.touchLocked(job.ID, func(j *Job) {
			if j.Status != StatusRunning {
				return
			}
			if status != "" {
				j.Detail = status
			}
			if ratio > j.Ratio && ratio <= 1 {
				j.Ratio = ratio
			}
		})
		s.mu.Unlock()
		s.ping()
	}
	var file, text string
	var err error
	if s.runner == nil {
		err = fmt.Errorf("no runner")
	} else {
		file, text, err = s.runner(ctx, job.Kind, job.Prompt, report)
	}
	s.mu.Lock()
	if cancel := s.running[job.ID]; cancel != nil {
		delete(s.running, job.ID)
	}
	s.touchLocked(job.ID, func(j *Job) {
		j.Ended = nowMS()
		if ctx.Err() != nil {
			j.Status = StatusCanceled
			j.Detail = "canceled"
			j.Err = "canceled"
			return
		}
		if err != nil {
			j.Status = StatusFailed
			j.Detail = "failed"
			j.Err = err.Error()
			j.Ratio = 0
			return
		}
		j.Status = StatusReady
		j.Detail = "ready"
		j.Ratio = 1
		j.File = file
		j.Text = text
	})
	for _, j := range s.jobs {
		if j.ID == job.ID {
			s.deliverLocked(j)
			break
		}
	}
	s.mu.Unlock()
	s.ping()
}

func (s *Stage) touchLocked(id string, fn func(*Job)) {
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			fn(&s.jobs[i])
			return
		}
	}
}

func (s *Stage) deliverLocked(job Job) {
	waiters := s.waiters[job.ID]
	delete(s.waiters, job.ID)
	for _, ch := range waiters {
		ch <- job
	}
}

func (s *Stage) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Stage) ping() {
	if s == nil || s.changed == nil {
		return
	}
	s.changed()
}

func terminal(status string) bool {
	switch status {
	case StatusReady, StatusFailed, StatusCanceled:
		return true
	default:
		return false
	}
}

// QueueView is the companion viewer's copy of the queue.
// Jobs are newest first, the same order Glance describes.
type QueueView struct {
	Available bool   `json:"available"`
	Open      bool   `json:"open"`
	Layout    string `json:"layout"`
	Feature   string `json:"feature,omitempty"`
	Media     string `json:"media,omitempty"`
	Jobs      []Job  `json:"jobs"`
}

// QueueView copies the queue for the companion viewer. media is the stage
// page origin, so a finished file can be shown from there.
func (s *Stage) QueueView(open bool, media string) QueueView {
	if s == nil {
		return QueueView{Jobs: []Job{}}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := newestJobs(s.jobs)
	feature := s.feature
	if feature == "" {
		feature = s.latestLocked()
	}
	return QueueView{
		Available: true,
		Open:      open,
		Layout:    s.layout,
		Feature:   feature,
		Media:     strings.TrimRight(strings.TrimSpace(media), "/"),
		Jobs:      jobs,
	}
}

func newestJobs(jobs []Job) []Job {
	out := append([]Job(nil), jobs...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if out == nil {
		out = []Job{}
	}
	return out
}

// View is the JSON the page renders. Same fields Glance talks about.
func (s *Stage) View() any {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := newestJobs(s.jobs)
	feature := s.feature
	if feature == "" {
		feature = s.latestLocked()
	}
	controls := append([]Control(nil), s.controls...)
	return map[string]any{
		"kind":     "stage",
		"layout":   s.layout,
		"feature":  feature,
		"jobs":     jobs,
		"theme":    s.theme,
		"controls": controls,
		"now":      nowMS(),
	}
}

// Glance describes the page the browser paints: a dark desk, a left
// column of cards, and a right frame. Covered means the cards are not
// on screen; she can still feel the queue behind the cover.
func (s *Stage) Glance(open bool) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	if !open {
		b.WriteString("The stage window is covered: an empty frame, the words that it is closed, no cards on screen. ")
	} else {
		switch s.layout {
		case LayoutQueue:
			b.WriteString("The stage window is open. Layout is queue: only the left column of job cards, no right frame. ")
		case LayoutStage:
			b.WriteString("The stage window is open. Layout is stage: only the right frame, no card column. ")
		default:
			b.WriteString("The stage window is open. Layout is split: a left column of job cards and a right frame. ")
		}
	}
	if len(s.jobs) == 0 {
		b.WriteString("The queue is empty. ")
		return s.finishGlance(&b)
	}
	if !open {
		b.WriteString("Behind the cover you can feel the queue, newest first: ")
	} else {
		b.WriteString("Queue, newest first: ")
	}
	start := 0
	if len(s.jobs) > 6 {
		start = len(s.jobs) - 6
	}
	shown := append([]Job(nil), s.jobs[start:]...)
	for i, j := 0, len(shown)-1; i < j; i, j = i+1, j-1 {
		shown[i], shown[j] = shown[j], shown[i]
	}
	for i, j := range shown {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s %s %s", j.ID, j.Kind, j.Status)
		if j.Status == StatusRunning {
			fmt.Fprintf(&b, " %.0f%%", j.Ratio*100)
		}
		if p := clip(j.Prompt, 60); p != "" {
			fmt.Fprintf(&b, " (%s)", p)
		}
	}
	b.WriteString(". ")
	if open && s.layout == LayoutQueue {
		return s.finishGlance(&b)
	}
	feat := s.featuredLocked()
	if feat == nil {
		b.WriteString("The right frame is empty. ")
		return s.finishGlance(&b)
	}
	fmt.Fprintf(&b, "The right frame features %s, a %s card. ", feat.ID, feat.Kind)
	switch {
	case feat.Status == StatusReady && feat.Kind == KindImage && feat.File != "":
		b.WriteString("A still image fills the frame. ")
		if feat.Look != "" {
			fmt.Fprintf(&b, "The picture looks like: %s ", clip(feat.Look, 240))
		}
	case feat.Status == StatusReady && feat.Kind == KindVideo && feat.File != "":
		b.WriteString("A video player fills the frame. ")
	case feat.Status == StatusReady && (feat.Kind == KindSpeech || feat.Kind == KindSong) && feat.File != "":
		b.WriteString("An audio player sits in the frame. ")
	case feat.Status == StatusReady && feat.Text != "":
		fmt.Fprintf(&b, "A text note fills the frame: %s ", clip(feat.Text, 240))
	case feat.Status == StatusRunning:
		fmt.Fprintf(&b, "An amber progress bar is at %.0f%%. No finished result yet. ", feat.Ratio*100)
	case feat.Status == StatusFailed:
		b.WriteString("The card is marked failed. ")
	default:
		fmt.Fprintf(&b, "Status %s. ", feat.Status)
	}
	return s.finishGlance(&b)
}

func (s *Stage) finishGlance(b *strings.Builder) string {
	b.WriteString(s.dressLocked())
	return strings.TrimSpace(b.String())
}

func (s *Stage) dressLocked() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Decoration: ground %s, desk %s, ink %s, accent %s, %s letters, %s corners, %s frame. ",
		s.theme.Background, s.theme.Desk, s.theme.Ink, s.theme.Accent, s.theme.Font, s.theme.Radius, frameWord(s.theme.Frame))
	if len(s.controls) == 0 {
		b.WriteString("No added controls.")
		return b.String()
	}
	b.WriteString("Added controls under the title: ")
	for i, c := range s.controls {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s %q", c.Kind, clip(c.Text, 40))
		if c.Kind == ControlMeter {
			fmt.Fprintf(&b, " at %.0f%%", c.Value*100)
		}
	}
	b.WriteString(".")
	return b.String()
}

func frameWord(frame string) string {
	switch frame {
	case FramePlain:
		return "a plain"
	case FrameGlow:
		return "a glowing"
	default:
		return "a line"
	}
}

func (s *Stage) featuredLocked() *Job {
	id := s.feature
	if id == "" {
		id = s.latestLocked()
	}
	for i := range s.jobs {
		if s.jobs[i].ID == id {
			return &s.jobs[i]
		}
	}
	return nil
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
