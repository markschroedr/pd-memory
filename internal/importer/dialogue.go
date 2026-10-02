package importer

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/markschroedr/pd-memory/internal/memory"
)

type Message struct {
	ID        string `json:"id,omitempty"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
	Speaker   string `json:"speaker,omitempty"`
	Complete  bool   `json:"-"`
}
type Dialogue struct {
	Text         string   `json:"text"`
	Participants []string `json:"participants"`
	Stats        struct {
		UserChars      int `json:"userChars"`
		AssistantChars int `json:"assistantChars"`
		Truncated      int `json:"truncatedAssistantMessages"`
	} `json:"stats"`
}

var blankLines = regexp.MustCompile(`\n{3,}`)

func Prepare(messages []Message) (Dialogue, error) {
	var d Dialogue
	d.Participants = []string{}
	pieces := []string{}
	user := false
	for _, m := range messages {
		if m.Role != "user" && m.Role != "assistant" {
			return d, fmt.Errorf("invalid dialogue role")
		}
		if m.Timestamp != "" {
			if e := memory.Date(&m.Timestamp); e != nil {
				return d, e
			}
		}
		text := strings.TrimSpace(blankLines.ReplaceAllString(strings.ReplaceAll(strings.ReplaceAll(m.Text, "\x00", ""), "\r\n", "\n"), "\n\n"))
		if text == "" {
			continue
		}
		limit := 5000
		if m.Role == "user" {
			limit = 100000
			user = true
		}
		units := utf16.Encode([]rune(text))
		if len(units) > limit {
			n := limit - 5
			head := n / 3
			text = strings.TrimRight(string(utf16.Decode(units[:head])), " \t\n") + "\n...\n" + strings.TrimLeft(string(utf16.Decode(units[len(units)-(n-head):])), " \t\n")
			if m.Role == "assistant" {
				d.Stats.Truncated++
			}
		}
		count := len(utf16.Encode([]rune(text)))
		if m.Role == "user" {
			d.Stats.UserChars += count
		} else {
			d.Stats.AssistantChars += count
		}
		speaker := m.Speaker
		if speaker == "" {
			if m.Role == "user" {
				speaker = "User"
			} else {
				speaker = "Assistant"
			}
		}
		found := false
		for _, p := range d.Participants {
			if p == speaker {
				found = true
			}
		}
		if !found {
			d.Participants = append(d.Participants, speaker)
		}
		prefix := speaker
		if m.Timestamp != "" {
			prefix = m.Timestamp + " " + speaker
		}
		pieces = append(pieces, prefix+":\n"+text)
	}
	if !user {
		return d, fmt.Errorf("dialogue requires a user message")
	}
	d.Text = strings.Join(pieces, "\n\n")
	return d, nil
}

var speakerLabels = regexp.MustCompile(`(?m)^([^:\n]{1,80}):\s`)

func Speakers(text string) []string {
	out := []string{}
	for _, m := range speakerLabels.FindAllStringSubmatch(text, -1) {
		name := strings.TrimSpace(m[1])
		found := false
		for _, p := range out {
			if p == name {
				found = true
			}
		}
		if !found {
			out = append(out, name)
		}
	}
	return out
}
