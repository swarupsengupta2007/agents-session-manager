package agents

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agents-session-manager/internal/migrate"
	"agents-session-manager/internal/model"
)

const (
	TransferExport  = "export"
	TransferMigrate = "migrate"
)

// Message is one user/assistant turn extracted from a native transcript.
type Message struct {
	Role  string // user | assistant
	Text  string
	Time  time.Time
	Model string
}

// Transcript is the portable session we copy between agents.
type Transcript struct {
	Source   model.Session
	Title    string
	Cwd      string
	Model    string
	Created  time.Time
	Updated  time.Time
	Messages []Message
}

func (t *Transcript) ensureMeta() {
	if t.Title == "" {
		for _, m := range t.Messages {
			if m.Role == "user" && strings.TrimSpace(m.Text) != "" {
				t.Title = truncate(strings.Join(strings.Fields(m.Text), " "), 80)
				break
			}
		}
	}
	if t.Title == "" {
		t.Title = t.Source.Title
	}
	if t.Title == "" {
		t.Title = t.Source.ID
	}
	if t.Cwd == "" {
		t.Cwd = t.Source.Cwd
	}
	if t.Model == "" {
		t.Model = t.Source.Model
	}
	if t.Created.IsZero() {
		t.Created = t.Source.CreatedAt
	}
	if t.Created.IsZero() {
		t.Created = time.Now().UTC()
	}
	if t.Updated.IsZero() {
		t.Updated = t.Source.UpdatedAt
	}
	if t.Updated.IsZero() {
		t.Updated = t.Created
	}
	if t.Model == "" {
		for i := len(t.Messages) - 1; i >= 0; i-- {
			if t.Messages[i].Model != "" {
				t.Model = t.Messages[i].Model
				break
			}
		}
	}
}

func (t *Transcript) add(role, text, model string, ts time.Time) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if role != "user" && role != "assistant" {
		return
	}
	if skipExportText(text) {
		return
	}
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	t.Messages = append(t.Messages, Message{Role: role, Text: text, Time: ts, Model: model})
}

func skipExportText(s string) bool {
	switch {
	case strings.HasPrefix(s, "<user_info>"),
		strings.HasPrefix(s, "<system-reminder>"),
		strings.HasPrefix(s, "<local-command-caveat>"),
		strings.Contains(s, "<command-name>"),
		strings.Contains(s, "<local-command-stdout>"):
		return true
	default:
		return false
	}
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func jsonText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if s := jsonText(p); s != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(s)
			}
		}
		return b.String()
	case map[string]any:
		if s, ok := t["text"].(string); ok && s != "" {
			return s
		}
		if c, ok := t["content"]; ok {
			return jsonText(c)
		}
		if c, ok := t["parts"]; ok {
			return jsonText(c)
		}
		if c, ok := t["message"]; ok {
			return jsonText(c)
		}
	}
	return ""
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Time{}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func mustJSONLine(v any) string {
	return mustJSON(v) + "\n"
}

// ExtractTranscript reads the portable turns out of a native session.
func ExtractTranscript(a Agent, s model.Session) (*Transcript, error) {
	var (
		t   *Transcript
		err error
	)
	switch x := a.(type) {
	case *Claude:
		t, err = x.Extract(s)
	case *Codex:
		t, err = x.Extract(s)
	case *Grok:
		t, err = x.Extract(s)
	case *Agy:
		t, err = x.Extract(s)
	case *Qwen:
		t, err = x.Extract(s)
	case *Muse:
		t, err = x.Extract(s)
	default:
		return nil, fmt.Errorf("extract: unsupported agent %s", a.Kind())
	}
	if err != nil {
		return nil, err
	}
	t.Source = s
	t.ensureMeta()
	if len(t.Messages) == 0 {
		if s.Title != "" {
			t.add("user", s.Title, s.Model, s.CreatedAt)
		}
	}
	if len(t.Messages) == 0 {
		return nil, fmt.Errorf("session %s has no exportable messages", s.ID)
	}
	t.ensureMeta()
	return t, nil
}

func importPlan(a Agent, t *Transcript, newID string) (*migrate.Plan, error) {
	switch x := a.(type) {
	case *Claude:
		return x.ImportPlan(t, newID)
	case *Codex:
		return x.ImportPlan(t, newID)
	case *Grok:
		return x.ImportPlan(t, newID)
	case *Agy:
		return x.ImportPlan(t, newID)
	case *Qwen:
		return x.ImportPlan(t, newID)
	case *Muse:
		return x.ImportPlan(t, newID)
	default:
		return nil, fmt.Errorf("import: unsupported agent %s", a.Kind())
	}
}

// TransferPlan copies (export) or copies-then-archives (migrate) s from src to dst.
func TransferPlan(src, dst Agent, s model.Session, mode string) (*migrate.Plan, error) {
	switch mode {
	case TransferExport, TransferMigrate:
	default:
		return nil, fmt.Errorf("mode must be export or migrate")
	}
	if src.Kind() == dst.Kind() && SamePath(src.Root(), dst.Root()) && mode == TransferMigrate {
		return nil, fmt.Errorf("cannot migrate a session onto the same %s store; export to copy it", src.Kind())
	}
	if mode == TransferMigrate {
		if err := guardIfActive(src, []model.Session{s}); err != nil {
			return nil, err
		}
	}
	t, err := ExtractTranscript(src, s)
	if err != nil {
		return nil, err
	}
	newID := newSessionID()
	plan, err := importPlan(dst, t, newID)
	if err != nil {
		return nil, err
	}
	plan.Agent = string(dst.Kind())
	plan.Transfer = mode
	plan.FromKind = string(src.Kind())
	plan.ToKind = string(dst.Kind())
	plan.NewID = newID
	plan.NewTitle = t.Title
	plan.LockKinds = []string{string(src.Kind()), string(dst.Kind())}
	if mode == TransferMigrate {
		del, err := src.DeletePlan([]model.Session{s})
		if err != nil {
			return nil, err
		}
		for _, a := range del.Actions {
			plan.Add(a)
		}
	}
	if plan.Empty() {
		return nil, fmt.Errorf("no actions planned")
	}
	return plan, nil
}

func guardIfActive(a Agent, ss []model.Session) error {
	type g interface {
		guardActive([]model.Session) error
	}
	if x, ok := a.(g); ok {
		return x.guardActive(ss)
	}
	return nil
}
