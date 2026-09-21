package desk

import (
	"encoding/json"
	"strings"
)

func parseTextJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	s = strings.TrimSpace(s)
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i >= 0 && j > i {
		var obj struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal([]byte(s[i:j+1]), &obj); err == nil && obj.Text != nil {
			return strings.TrimSpace(*obj.Text)
		}
	}
	if s == "" || strings.HasPrefix(strings.ToLower(s), "sorry") {
		return ""
	}
	return strings.TrimSpace(s)
}
