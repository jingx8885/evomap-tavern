package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseURL is the default Akasha new-api gateway.
const DefaultBaseURL = "https://newapi.1234bot.com/v1"

// DefaultJevModel is the System One judgment model alias.
const DefaultJevModel = "jev-latest"

// DefaultLiveModel is the gpt-live duplex voice model alias.
const DefaultLiveModel = "gpt-live-1-boulder-alpha"

// DefaultPlannerModel is the LLM used for long-term planning.
const DefaultPlannerModel = "gpt-4o-mini"

// ResolveBaseURL returns the OpenAI-compatible /v1 root:
// explicit value > NEW_API_BASE_URL > default. It normalizes a bare host
// or a path root to ".../v1".
func ResolveBaseURL(explicit string) string {
	v := strings.TrimSpace(explicit)
	if v == "" {
		v = strings.TrimSpace(os.Getenv("NEW_API_BASE_URL"))
	}
	if v == "" {
		v = DefaultBaseURL
	}
	return NormalizeBaseURL(v)
}

// NormalizeBaseURL maps host / /v1 / path roots to ".../v1".
func NormalizeBaseURL(v string) string {
	v = strings.TrimSpace(v)
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return strings.TrimRight(v, "/")
	}
	path := strings.TrimRight(u.Path, "/")
	switch {
	case path == "":
		path = "/v1"
	case !strings.HasSuffix(path, "/v1"):
		path += "/v1"
	}
	u.Path = path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// ResolveAPIKey returns the first usable credential:
// OPENAI_API_KEY > LOVBROWSER_API_KEY > NEW_API_API_KEY >
// LOVBROWSER_API_KEY in ~/.config/akasha/credentials.env.
func ResolveAPIKey() (string, error) {
	for _, name := range []string{"OPENAI_API_KEY", "LOVBROWSER_API_KEY", "NEW_API_API_KEY"} {
		if k := strings.TrimSpace(os.Getenv(name)); k != "" {
			return k, nil
		}
	}
	cred := filepath.Join(os.Getenv("HOME"), ".config", "akasha", "credentials.env")
	if f, err := os.Open(cred); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "LOVBROWSER_API_KEY=") {
				k := strings.TrimSpace(strings.TrimPrefix(line, "LOVBROWSER_API_KEY="))
				k = strings.Trim(k, "\"'")
				if k != "" {
					return k, nil
				}
			}
		}
	}
	return "", fmt.Errorf("no API key: set LOVBROWSER_API_KEY or OPENAI_API_KEY " +
		"(see akasha-grimoire skills/akasha-key-setup for device-code setup)")
}
