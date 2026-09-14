package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// pluginConfig is the parsed plugins.configs.<commandcode-go> block.
//
// The canonical multi-key shape is:
//
//	api_keys:
//	  - key: user_...
//	    weight: 10
//
// The legacy `api_key: user_...` form is accepted and treated as a single-member
// pool so existing configs keep working. Key rotation is used only for failures
// that happen before the upstream produced a response.
type pluginConfig struct {
	mu sync.RWMutex

	APIKeys        []poolMember `yaml:"api_keys"`
	APIKey         string       `yaml:"api_key"`
	BaseURL        string       `yaml:"base_url"`
	CLIVersion     string       `yaml:"cli_version"`
	ProjectSlug    string       `yaml:"project_slug"`
	ZDR            bool         `yaml:"zdr"`
	WorkingDir     string       `yaml:"working_dir"`
	PermissionMode string       `yaml:"permission_mode"`
	Models         []modelMap   `yaml:"models"`

	// Prefix and model registration settings
	Prefix            string `yaml:"prefix"`              // e.g. "cmdc", default "cmdc"
	ShowPrefix        *bool  `yaml:"show_prefix"`         // default true
	IncludeBareModels bool   `yaml:"include_bare_models"` // default false (only show prefixed models)

	// Reasoning replay mode: "both" (default), "standard", "inject", "off"
	ReasoningReplay string `yaml:"reasoning_replay"`

	indexOnce sync.Once
	byAlias   map[string]string
	claims    map[string]struct{}
	display   map[string]string
}

// poolMember is one credential in the rotation pool.
type poolMember struct {
	Key      string `yaml:"key" json:"key"`
	Weight   int    `yaml:"weight" json:"weight"`
	Label    string `yaml:"label,omitempty" json:"label,omitempty"`
	Disabled bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// modelMap maps a client-facing alias to an upstream vendor model id and can
// override the advertised display name of that upstream id.
type modelMap struct {
	Alias       string `yaml:"alias"`
	Name        string `yaml:"name"`
	DisplayName string `yaml:"display_name"`
}

func parseConfig(raw []byte) *pluginConfig {
	cfg := &pluginConfig{}
	if len(raw) > 0 {
		_ = yaml.Unmarshal(raw, cfg)
	}
	return cfg
}

func (c *pluginConfig) baseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if value := strings.TrimSpace(c.BaseURL); value != "" {
		return value
	}
	return defaultBaseURL
}

func (c *pluginConfig) cliVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if value := strings.TrimSpace(c.CLIVersion); value != "" {
		return value
	}
	return defaultCLIVersion
}

func (c *pluginConfig) projectSlug() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if value := strings.TrimSpace(c.ProjectSlug); value != "" {
		return value
	}
	return defaultProjectSlug
}

func (c *pluginConfig) zdrEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ZDR
}

func (c *pluginConfig) permissionMode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if value := strings.TrimSpace(c.PermissionMode); value != "" {
		return value
	}
	return "default"
}

func (c *pluginConfig) workingDir() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if value := strings.TrimSpace(c.WorkingDir); value != "" {
		return value
	}
	return "."
}

func (c *pluginConfig) prefix() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	p := strings.TrimSpace(c.Prefix)
	if p == "" {
		return "cmdc"
	}
	return strings.TrimSuffix(p, "/")
}

func (c *pluginConfig) showPrefix() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.ShowPrefix == nil {
		return true
	}
	return *c.ShowPrefix
}

func (c *pluginConfig) includeBareModels() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.IncludeBareModels
}

func (c *pluginConfig) reasoningReplay() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	mode := strings.ToLower(strings.TrimSpace(c.ReasoningReplay))
	if mode == "" {
		return "both"
	}
	return mode
}

// members returns the configured key pool, falling back to the legacy single key.
func (c *pluginConfig) members() []poolMember {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]poolMember, 0, len(c.APIKeys)+1)
	for _, member := range c.APIKeys {
		key := strings.TrimSpace(member.Key)
		if key == "" {
			continue
		}
		if member.Weight <= 0 {
			member.Weight = 1
		}
		member.Key = key
		out = append(out, member)
	}
	if len(out) == 0 {
		if key := strings.TrimSpace(c.APIKey); key != "" {
			out = append(out, poolMember{Key: key, Weight: 1, Label: "default"})
		}
	}
	return out
}

// wireConfig builds the envelope's `config` block.
func (c *pluginConfig) wireConfig() wireConfig {
	return wireConfig{
		WorkingDir:    c.workingDir(),
		Date:          time.Now().UTC().Format("2006-01-02"),
		Environment:   runtime.GOOS,
		Structure:     []any{},
		IsGitRepo:     false,
		CurrentBranch: "",
		MainBranch:    "",
		GitStatus:     "",
		RecentCommits: []string{},
	}
}

func (c *pluginConfig) ensureIndexes() {
	c.indexOnce.Do(func() {
		c.byAlias = make(map[string]string)
		c.claims = make(map[string]struct{})
		c.display = make(map[string]string)
		for _, entry := range c.Models {
			alias := strings.TrimSpace(entry.Alias)
			name := strings.TrimSpace(entry.Name)
			if name == "" {
				name = alias
			}
			if alias != "" && name != "" {
				c.byAlias[alias] = name
				c.claims[alias] = struct{}{}
			}
			if name != "" {
				c.claims[name] = struct{}{}
				if display := strings.TrimSpace(entry.DisplayName); display != "" {
					c.display[name] = display
				}
			}
		}
	})
}

// upstreamModel resolves a client-facing name to the upstream vendor model id,
// stripping the plugin namespace the host may have added.
func (c *pluginConfig) upstreamModel(model string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return trimmed
	}
	pfx := c.prefix()
	if rest, ok := strings.CutPrefix(trimmed, pfx+"/"); ok && rest != "" {
		trimmed = rest
	}
	if rest, ok := strings.CutPrefix(trimmed, "cmdc/"); ok && rest != "" {
		trimmed = rest
	}
	if rest, ok := strings.CutPrefix(trimmed, "commandcode-go/"); ok && rest != "" {
		trimmed = rest
	}
	c.ensureIndexes()
	if name, ok := c.byAlias[trimmed]; ok && name != "" {
		return name
	}
	return trimmed
}

// claimsModel reports whether the plugin should serve a bare model identifier.
func (c *pluginConfig) claimsModel(model string) bool {
	if model == "" {
		return false
	}
	c.ensureIndexes()
	if _, ok := c.claims[model]; ok {
		return true
	}
	if c.includeBareModels() || !c.showPrefix() {
		return catalogHasModel(model)
	}
	return false
}

func (c *pluginConfig) displayNameOverrides() map[string]string {
	c.ensureIndexes()
	out := make(map[string]string, len(c.display))
	for id, name := range c.display {
		out[id] = name
	}
	return out
}

// catalogHasModel reports whether the embedded catalog advertises the id.
func catalogHasModel(model string) bool {
	if model == "" {
		return false
	}
	for _, entry := range cachedCatalog() {
		if entry.ID == model {
			return true
		}
	}
	return false
}

var (
	catalogOnce  sync.Once
	catalogCache []catalogEntry
)

func cachedCatalog() []catalogEntry {
	catalogOnce.Do(func() {
		_ = json.Unmarshal([]byte(catalogJSON), &catalogCache)
	})
	return catalogCache
}

// newSessionID returns a UUIDv4 for the x-session-id header.
func newSessionID() string {
	var buf [16]byte
	if _, errRead := rand.Read(buf[:]); errRead != nil {
		return fmt.Sprintf("sess-%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	raw := hex.EncodeToString(buf[:])
	return raw[0:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:32]
}

// jsonUnmarshal is a thin indirection: an empty payload is reported as an error
// instead of a silent zero value, so callers can fall back deliberately.
func jsonUnmarshal(raw []byte, v any) error {
	if len(raw) == 0 {
		return fmt.Errorf("empty payload")
	}
	return json.Unmarshal(raw, v)
}
