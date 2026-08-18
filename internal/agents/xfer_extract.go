package agents

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"agents-session-manager/internal/model"
)

func (c *Claude) Extract(s model.Session) (*Transcript, error) {
	t := &Transcript{Title: s.Title, Cwd: s.Cwd, Model: s.Model, Created: s.CreatedAt, Updated: s.UpdatedAt}
	f, err := os.Open(s.File)
	if err != nil {
		return t, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if meta, _ := rec["isMeta"].(bool); meta {
			continue
		}
		typ, _ := rec["type"].(string)
		if typ != "user" && typ != "assistant" {
			continue
		}
		text := jsonText(rec["message"])
		if text == "" {
			text = jsonText(rec["content"])
		}
		modelName := ""
		if typ == "assistant" {
			modelName = claudeModelOf(rec)
		}
		t.add(typ, text, modelName, parseTime(asString(rec["timestamp"])))
	}
	return t, sc.Err()
}

func (c *Codex) Extract(s model.Session) (*Transcript, error) {
	t := &Transcript{Title: s.Title, Cwd: s.Cwd, Model: s.Model, Created: s.CreatedAt, Updated: s.UpdatedAt}
	f, err := os.Open(s.File)
	if err != nil {
		return t, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var env struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Message string `json:"message"`
				Content any    `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &env) != nil {
			continue
		}
		ts := parseTime(env.Timestamp)
		switch {
		case env.Type == "event_msg" && env.Payload.Type == "user_message":
			t.add("user", env.Payload.Message, "", ts)
		case env.Type == "event_msg" && env.Payload.Type == "agent_message":
			t.add("assistant", env.Payload.Message, "", ts)
		case env.Type == "response_item" && (env.Payload.Type == "message" || env.Payload.Type == ""):
			role := env.Payload.Role
			if role == "user" || role == "assistant" {
				t.add(role, jsonText(env.Payload.Content), "", ts)
			}
		}
	}
	return t, sc.Err()
}

func (g *Grok) Extract(s model.Session) (*Transcript, error) {
	t := &Transcript{Title: s.Title, Cwd: s.Cwd, Model: s.Model, Created: s.CreatedAt, Updated: s.UpdatedAt}
	path := s.File
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "chat_history.jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		return t, nil // title-only is fine; TransferPlan fills from Title
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		switch typ {
		case "user":
			t.add("user", jsonText(rec["content"]), "", time.Time{})
		case "assistant":
			t.add("assistant", jsonText(rec["content"]), t.Model, time.Time{})
		}
	}
	return t, sc.Err()
}

func (a *Agy) Extract(s model.Session) (*Transcript, error) {
	t := &Transcript{Title: s.Title, Cwd: s.Cwd, Model: s.Model, Created: s.CreatedAt, Updated: s.UpdatedAt}
	// Conversation bodies are protobuf; listing metadata is all we can
	// export without corrupting the db.
	if s.Title != "" {
		t.add("user", s.Title, "", s.CreatedAt)
	}
	return t, nil
}

func (q *Qwen) Extract(s model.Session) (*Transcript, error) {
	t := &Transcript{Title: s.Title, Cwd: s.Cwd, Model: s.Model, Created: s.CreatedAt, Updated: s.UpdatedAt}
	path := s.File
	if strings.HasSuffix(path, ".runtime.json") {
		return t, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return t, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		typ, _ := rec["type"].(string)
		if typ != "user" && typ != "assistant" {
			continue
		}
		text := jsonText(rec["message"])
		modelName, _ := rec["model"].(string)
		t.add(typ, text, modelName, parseTime(asString(rec["timestamp"])))
	}
	return t, sc.Err()
}

func (m *Muse) Extract(s model.Session) (*Transcript, error) {
	t := &Transcript{Title: s.Title, Cwd: s.Cwd, Model: s.Model, Created: s.CreatedAt, Updated: s.UpdatedAt}
	path := s.File
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "session.jsonl")
	}
	f, err := os.Open(path)
	if err != nil {
		return t, nil
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte("user_intent")) && !bytes.Contains(line, []byte("model_messages")) {
			continue
		}
		var rec struct {
			PayloadType string `json:"payload_type"`
			RecordedAt  int64  `json:"recorded_at"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		ts := time.Time{}
		if rec.RecordedAt > 0 {
			ts = time.UnixMicro(rec.RecordedAt)
		}
		if strings.Contains(rec.PayloadType, "user_intent.accepted") {
			t.add("user", musePromptFromLine(line), "", ts)
		}
	}
	return t, sc.Err()
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
