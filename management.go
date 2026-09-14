package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

//go:embed admin.html
var adminHTML []byte

type rpcManagementRegistrationResponse struct {
	Routes    []pluginapi.ManagementRoute `json:"routes,omitempty"`
	Resources []pluginapi.ResourceRoute   `json:"resources,omitempty"`
}

type managementService struct {
	mu            sync.Mutex
	cfg           *pluginConfig
	pendingStates map[string]time.Time
}

var (
	globalMgmt *managementService
	mgmtOnce   sync.Once
)

func getManagementService(cfg *pluginConfig) *managementService {
	mgmtOnce.Do(func() {
		globalMgmt = &managementService{
			cfg:           cfg,
			pendingStates: make(map[string]time.Time),
		}
		globalMgmt.syncAuthDir()
	})
	if cfg != nil {
		globalMgmt.cfg = cfg
		globalMgmt.syncAuthDir()
	}
	return globalMgmt
}

func (m *managementService) RegisterManagement(_ context.Context, _ pluginapi.ManagementRegistrationRequest) (rpcManagementRegistrationResponse, error) {
	resources := []pluginapi.ResourceRoute{
		{
			Path:        "/admin",
			Menu:        "Command Code",
			Description: "Command Code 多账户授权与加权轮询管理 (支持 Go 及更高级套餐)",
		},
		{
			Path:        "/status",
			Description: "CommandCode accounts status",
		},
		{
			Path:        "/callback",
			Description: "CommandCode OAuth redirect callback handler",
		},
		{
			Path:        "/api/auth/start",
			Description: "Generate auth URL",
		},
		{
			Path:        "/api/accounts",
			Description: "Accounts list, add and delete API",
		},
		{
			Path:        "/api/settings",
			Description: "Model prefix and registration settings API",
		},
	}
	return rpcManagementRegistrationResponse{
		Resources: resources,
	}, nil
}

func (m *managementService) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	path := req.Path
	if strings.HasSuffix(path, "/admin") || strings.HasSuffix(path, "/status") {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type": []string{"text/html; charset=utf-8"},
			},
			Body: adminHTML,
		}, nil
	}

	if strings.HasSuffix(path, "/callback") {
		return m.handleCallback(req)
	}

	if strings.HasSuffix(path, "/api/auth/start") {
		return m.handleAuthStart(req)
	}

	if strings.HasSuffix(path, "/api/accounts") {
		return m.handleAccounts(req)
	}

	if strings.HasSuffix(path, "/api/settings") {
		return m.handleSettings(req)
	}

	return pluginapi.ManagementResponse{
		StatusCode: http.StatusNotFound,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       []byte(`{"error":"not_found"}`),
	}, nil
}

func (m *managementService) generateState() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	state := hex.EncodeToString(b)
	m.pendingStates[state] = time.Now().Add(15 * time.Minute)

	now := time.Now()
	for k, exp := range m.pendingStates {
		if now.After(exp) {
			delete(m.pendingStates, k)
		}
	}
	return state
}

func (m *managementService) handleAuthStart(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	state := m.generateState()
	// Command Code 官方鉴权机制要求 callback 必须是 localhost / 127.0.0.1
	// 授权成功后前端在无法直连 127.0.0.1 时会自动跳至官方 fallback 页面呈现 API Key 供一键复制
	authURL := fmt.Sprintf("https://commandcode.ai/studio/auth/cli?callback=%s&state=%s",
		url.QueryEscape("http://127.0.0.1:5959/callback"),
		url.QueryEscape(state),
	)

	body, _ := json.Marshal(map[string]any{
		"auth_url": authURL,
		"keys_url": "https://commandcode.ai/keys",
		"state":    state,
	})
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}, nil
}

func (m *managementService) handleCallback(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	apiKey := strings.TrimSpace(req.Query.Get("apiKey"))
	userName := strings.TrimSpace(req.Query.Get("userName"))
	userId := strings.TrimSpace(req.Query.Get("userId"))
	keyName := strings.TrimSpace(req.Query.Get("keyName"))
	errParam := strings.TrimSpace(req.Query.Get("error"))
	errDesc := strings.TrimSpace(req.Query.Get("error_description"))

	if errParam != "" {
		msg := errDesc
		if msg == "" {
			msg = errParam
		}
		target := "/v0/resource/plugins/commandcode-go/admin?auth=error&error=" + url.QueryEscape(msg)
		return redirectResponse(target), nil
	}

	if apiKey == "" {
		target := "/v0/resource/plugins/commandcode-go/admin?auth=error&error=" + url.QueryEscape("未收到授权凭证")
		return redirectResponse(target), nil
	}

	label := userName
	if label == "" {
		label = keyName
	}
	if label == "" {
		label = userId
	}
	if label == "" {
		if len(apiKey) > 10 {
			label = "user-" + apiKey[len(apiKey)-6:]
		} else {
			label = "account"
		}
	}

	m.addAccount(apiKey, 1, label)

	target := fmt.Sprintf("/v0/resource/plugins/commandcode-go/admin?auth=success&name=%s", url.QueryEscape(label))
	return redirectResponse(target), nil
}

func redirectResponse(target string) pluginapi.ManagementResponse {
	html := fmt.Sprintf(`<!DOCTYPE html><html><head><meta http-equiv="refresh" content="1;url=%s"></head><body style="background:#0b0f19;color:#fff;text-align:center;padding:50px;font-family:sans-serif;"><h3>正在跳转...</h3><p><a href="%s" style="color:#38bdf8;">点击此处直接返回</a></p></body></html>`, target, target)
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusSeeOther,
		Headers: http.Header{
			"Location":     []string{target},
			"Content-Type": []string{"text/html; charset=utf-8"},
		},
		Body: []byte(html),
	}
}

func (m *managementService) handleAccounts(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	op := strings.TrimSpace(req.Query.Get("op"))
	if op == "" || op == "list" {
		list := m.listAccounts()
		data, _ := json.Marshal(map[string]any{
			"accounts": list,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	if op == "add" {
		input := strings.TrimSpace(req.Query.Get("input"))
		label := strings.TrimSpace(req.Query.Get("label"))
		key := parseAPIKeyFromInput(input)
		if key == "" {
			return jsonError("未能从输入中提取出有效的 user_ API Key")
		}
		if label == "" {
			if len(key) > 10 {
				label = "user-" + key[len(key)-6:]
			} else {
				label = "account"
			}
		}
		weight := 1
		if w, err := strconv.Atoi(strings.TrimSpace(req.Query.Get("weight"))); err == nil && w > 0 {
			weight = w
		}
		m.addAccount(key, weight, label)
		data, _ := json.Marshal(map[string]any{
			"success": true,
			"name":    label,
			"weight":  weight,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	if op == "update" {
		key := strings.TrimSpace(req.Query.Get("key"))
		if key == "" {
			return jsonError("key 参数不能为空")
		}
		weight := 0
		if w, err := strconv.Atoi(strings.TrimSpace(req.Query.Get("weight"))); err == nil && w > 0 {
			weight = w
		}
		label := strings.TrimSpace(req.Query.Get("label"))
		var disabledPtr *bool
		if disStr := strings.TrimSpace(req.Query.Get("disabled")); disStr != "" {
			val := (disStr == "true" || disStr == "1")
			disabledPtr = &val
		}
		if err := m.updateAccount(key, weight, label, disabledPtr); err != nil {
			return jsonError(err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"success": true,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	if op == "toggle" {
		key := strings.TrimSpace(req.Query.Get("key"))
		if key == "" {
			return jsonError("key 参数不能为空")
		}
		if err := m.toggleAccount(key); err != nil {
			return jsonError(err.Error())
		}
		data, _ := json.Marshal(map[string]any{
			"success": true,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	if op == "delete" {
		key := strings.TrimSpace(req.Query.Get("key"))
		if key == "" {
			return jsonError("key 参数不能为空")
		}
		m.removeAccount(key)
		data, _ := json.Marshal(map[string]any{
			"success": true,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	return jsonError("unknown op")
}

func (m *managementService) handleSettings(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	op := strings.TrimSpace(req.Query.Get("op"))
	if op == "" || op == "get" {
		m.cfg.mu.RLock()
		pfx := m.cfg.prefix()
		showPfx := m.cfg.showPrefix()
		incBare := m.cfg.includeBareModels()
		reasoningReplay := m.cfg.reasoningReplay()
		m.cfg.mu.RUnlock()

		data, _ := json.Marshal(map[string]any{
			"success":             true,
			"prefix":              pfx,
			"show_prefix":         showPfx,
			"include_bare_models": incBare,
			"reasoning_replay":    reasoningReplay,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	if op == "update" {
		pfx := strings.TrimSpace(req.Query.Get("prefix"))
		if pfx == "" {
			pfx = "cmdc"
		}
		pfx = strings.TrimSuffix(pfx, "/")

		showPfx := true
		if val := strings.TrimSpace(req.Query.Get("show_prefix")); val != "" {
			showPfx = (val == "true" || val == "1")
		}

		incBare := false
		if val := strings.TrimSpace(req.Query.Get("include_bare_models")); val != "" {
			incBare = (val == "true" || val == "1")
		}

		reasoningReplay := strings.ToLower(strings.TrimSpace(req.Query.Get("reasoning_replay")))
		if reasoningReplay == "" {
			reasoningReplay = "both"
		}

		m.cfg.mu.Lock()
		m.cfg.Prefix = pfx
		m.cfg.ShowPrefix = &showPfx
		m.cfg.IncludeBareModels = incBare
		m.cfg.ReasoningReplay = reasoningReplay
		m.cfg.mu.Unlock()

		if err := m.persistSettings(pfx, showPfx, incBare, reasoningReplay); err != nil {
			return jsonError(fmt.Sprintf("保存配置失败: %v", err))
		}

		data, _ := json.Marshal(map[string]any{
			"success":             true,
			"prefix":              pfx,
			"show_prefix":         showPfx,
			"include_bare_models": incBare,
			"reasoning_replay":    reasoningReplay,
		})
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
			Body:       data,
		}, nil
	}

	return jsonError("unknown op")
}

func jsonError(msg string) (pluginapi.ManagementResponse, error) {
	data, _ := json.Marshal(map[string]any{
		"success": false,
		"error":   msg,
	})
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       data,
	}, nil
}

func parseAPIKeyFromInput(input string) string {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "user_") && !strings.Contains(input, " ") && !strings.Contains(input, "?") && !strings.Contains(input, "&") {
		return input
	}
	if u, err := url.Parse(input); err == nil {
		if k := u.Query().Get("apiKey"); strings.HasPrefix(k, "user_") {
			return k
		}
	}
	idx := strings.Index(input, "user_")
	if idx >= 0 {
		end := idx + 5
		for end < len(input) && ((input[end] >= 'a' && input[end] <= 'z') || (input[end] >= 'A' && input[end] <= 'Z') || (input[end] >= '0' && input[end] <= '9') || input[end] == '-' || input[end] == '_') {
			end++
		}
		return input[idx:end]
	}
	return ""
}

func (m *managementService) listAccounts() []poolMember {
	if m.cfg == nil {
		return nil
	}
	return m.cfg.members()
}

func (m *managementService) addAccount(key string, weight int, label string) {
	if m.cfg == nil || strings.TrimSpace(key) == "" {
		return
	}
	if weight <= 0 {
		weight = 1
	}

	m.cfg.mu.Lock()
	found := false
	for i, acc := range m.cfg.APIKeys {
		if acc.Key == key {
			m.cfg.APIKeys[i].Weight = weight
			if label != "" {
				m.cfg.APIKeys[i].Label = label
			}
			found = true
			break
		}
	}
	if !found {
		m.cfg.APIKeys = append(m.cfg.APIKeys, poolMember{
			Key:    key,
			Weight: weight,
			Label:  label,
		})
	}
	m.cfg.mu.Unlock()

	m.persistAccounts()
}

func (m *managementService) updateAccount(key string, weight int, label string, disabledPtr *bool) error {
	if m.cfg == nil || strings.TrimSpace(key) == "" {
		return fmt.Errorf("invalid account")
	}
	m.cfg.mu.Lock()
	found := false
	for i, acc := range m.cfg.APIKeys {
		if acc.Key == key {
			if weight > 0 {
				m.cfg.APIKeys[i].Weight = weight
			}
			if label != "" {
				m.cfg.APIKeys[i].Label = label
			}
			if disabledPtr != nil {
				m.cfg.APIKeys[i].Disabled = *disabledPtr
			}
			found = true
			break
		}
	}
	if !found && m.cfg.APIKey == key {
		w := 1
		if weight > 0 {
			w = weight
		}
		l := "default"
		if label != "" {
			l = label
		}
		dis := false
		if disabledPtr != nil {
			dis = *disabledPtr
		}
		m.cfg.APIKeys = append(m.cfg.APIKeys, poolMember{
			Key:      key,
			Weight:   w,
			Label:    l,
			Disabled: dis,
		})
		m.cfg.APIKey = ""
		found = true
	}
	m.cfg.mu.Unlock()

	if !found {
		return fmt.Errorf("account not found")
	}
	m.persistAccounts()
	return nil
}

func (m *managementService) toggleAccount(key string) error {
	if m.cfg == nil || strings.TrimSpace(key) == "" {
		return fmt.Errorf("invalid account")
	}
	m.cfg.mu.Lock()
	found := false
	for i, acc := range m.cfg.APIKeys {
		if acc.Key == key {
			m.cfg.APIKeys[i].Disabled = !m.cfg.APIKeys[i].Disabled
			found = true
			break
		}
	}
	m.cfg.mu.Unlock()

	if !found {
		return fmt.Errorf("account not found")
	}
	m.persistAccounts()
	return nil
}

func (m *managementService) removeAccount(key string) {
	if m.cfg == nil || strings.TrimSpace(key) == "" {
		return
	}

	m.cfg.mu.Lock()
	filtered := make([]poolMember, 0, len(m.cfg.APIKeys))
	for _, acc := range m.cfg.APIKeys {
		if acc.Key != key {
			filtered = append(filtered, acc)
		}
	}
	m.cfg.APIKeys = filtered
	if m.cfg.APIKey == key {
		m.cfg.APIKey = ""
	}
	m.cfg.mu.Unlock()

	m.persistAccounts()
}

func findConfigFile() string {
	if env := strings.TrimSpace(os.Getenv("CPA_CONFIG_PATH")); env != "" {
		if _, err := os.Stat(env); err == nil {
			return env
		}
	}
	candidates := []string{
		"config.yaml",
		"./config.yaml",
		"/CLIProxyAPI/config.yaml",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, "cliproxyapi", "config.yaml"))
		candidates = append(candidates, filepath.Join(home, ".cli-proxy-api", "config.yaml"))
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func findAuthDir() string {
	if env := strings.TrimSpace(os.Getenv("CPA_AUTH_DIR")); env != "" {
		if fi, err := os.Stat(env); err == nil && fi.IsDir() {
			return env
		}
	}
	cfgFile := findConfigFile()
	if cfgFile != "" {
		if content, err := os.ReadFile(cfgFile); err == nil {
			var raw struct {
				AuthDir string `yaml:"auth-dir"`
			}
			if err := yaml.Unmarshal(content, &raw); err == nil && strings.TrimSpace(raw.AuthDir) != "" {
				dir := strings.TrimSpace(raw.AuthDir)
				if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
					return dir
				}
			}
		}
	}
	candidates := []string{
		"./auth",
		"auth",
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".cli-proxy-api"))
		candidates = append(candidates, filepath.Join(home, "cliproxyapi", "auth"))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			return c
		}
	}
	return ""
}

func authFileName(label, key string) string {
	clean := strings.TrimSpace(label)
	var b strings.Builder
	for _, r := range clean {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '@' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		h := sha256.Sum256([]byte(key))
		s = hex.EncodeToString(h[:3])
	}
	return fmt.Sprintf("cmdc-%s.json", s)
}

func (m *managementService) syncAuthDir() {
	authDir := findAuthDir()
	if authDir == "" {
		return
	}

	accounts := m.listAccounts()
	activeFiles := make(map[string]bool)
	usedNames := make(map[string]bool)

	for _, acc := range accounts {
		name := authFileName(acc.Label, acc.Key)
		if usedNames[name] {
			h := sha256.Sum256([]byte(acc.Key))
			shortHash := hex.EncodeToString(h[:3])
			name = fmt.Sprintf("cmdc-%s-%s.json", strings.TrimSuffix(name[5:], ".json"), shortHash)
		}
		usedNames[name] = true
		activeFiles[name] = true

		filePath := filepath.Join(authDir, name)
		payload := map[string]any{
			"type":     "commandcode",
			"email":    acc.Label,
			"label":    acc.Label,
			"key":      acc.Key,
			"api_key":  acc.Key,
			"disabled": acc.Disabled,
			"weight":   acc.Weight,
		}
		data, err := json.MarshalIndent(payload, "", "  ")
		if err == nil {
			_ = os.WriteFile(filePath, data, 0600)
		}
	}

	// Remove stale cmdc-*.json files not in activeFiles
	entries, err := os.ReadDir(authDir)
	if err == nil {
		for _, entry := range entries {
			n := entry.Name()
			if strings.HasPrefix(n, "cmdc-") && strings.HasSuffix(n, ".json") {
				if !activeFiles[n] {
					_ = os.Remove(filepath.Join(authDir, n))
				}
			}
		}
	}
}

func (m *managementService) persistAccounts() {
	cfgFile := findConfigFile()
	if cfgFile != "" {
		content, errRead := os.ReadFile(cfgFile)
		if errRead == nil {
			accounts := m.listAccounts()
			if updated, errUpdate := updateYamlAccounts(content, accounts); errUpdate == nil {
				_ = os.WriteFile(cfgFile, updated, 0600)
			}
		}
	}
	m.syncAuthDir()
}

func updateYamlAccounts(content []byte, accounts []poolMember) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("empty yaml")
	}
	doc := root.Content[0]

	var pluginsNode *yaml.Node
	for i := 0; i < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "plugins" {
			pluginsNode = doc.Content[i+1]
			break
		}
	}
	if pluginsNode == nil {
		return nil, fmt.Errorf("plugins node not found")
	}

	var configsNode *yaml.Node
	for i := 0; i < len(pluginsNode.Content); i += 2 {
		if pluginsNode.Content[i].Value == "configs" {
			configsNode = pluginsNode.Content[i+1]
			break
		}
	}
	if configsNode == nil {
		return nil, fmt.Errorf("configs node not found")
	}

	var ccNode *yaml.Node
	for i := 0; i < len(configsNode.Content); i += 2 {
		if configsNode.Content[i].Value == "commandcode-go" || configsNode.Content[i].Value == "cmdc" {
			ccNode = configsNode.Content[i+1]
			break
		}
	}
	if ccNode == nil {
		return nil, fmt.Errorf("plugin config node (commandcode-go or cmdc) not found")
	}

	var seqNode yaml.Node
	seqNode.Kind = yaml.SequenceNode
	seqNode.Style = 0
	for _, acc := range accounts {
		var itemNode yaml.Node
		itemNode.Kind = yaml.MappingNode

		keyK := &yaml.Node{Kind: yaml.ScalarNode, Value: "key"}
		keyV := &yaml.Node{Kind: yaml.ScalarNode, Value: acc.Key}
		weightK := &yaml.Node{Kind: yaml.ScalarNode, Value: "weight"}
		weightV := &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", acc.Weight)}
		itemNode.Content = append(itemNode.Content, keyK, keyV, weightK, weightV)
		if acc.Label != "" {
			labelK := &yaml.Node{Kind: yaml.ScalarNode, Value: "label"}
			labelV := &yaml.Node{Kind: yaml.ScalarNode, Value: acc.Label}
			itemNode.Content = append(itemNode.Content, labelK, labelV)
		}
		if acc.Disabled {
			disK := &yaml.Node{Kind: yaml.ScalarNode, Value: "disabled"}
			disV := &yaml.Node{Kind: yaml.ScalarNode, Value: "true"}
			itemNode.Content = append(itemNode.Content, disK, disV)
		}
		seqNode.Content = append(seqNode.Content, &itemNode)
	}

	newContent := make([]*yaml.Node, 0, len(ccNode.Content))
	apiKeysFound := false
	for i := 0; i < len(ccNode.Content); i += 2 {
		k := ccNode.Content[i].Value
		if k == "api_key" {
			continue
		}
		if k == "api_keys" {
			newContent = append(newContent, ccNode.Content[i], &seqNode)
			apiKeysFound = true
			continue
		}
		newContent = append(newContent, ccNode.Content[i], ccNode.Content[i+1])
	}
	if !apiKeysFound {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: "api_keys"}
		newContent = append(newContent, keyNode, &seqNode)
	}
	ccNode.Content = newContent

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (m *managementService) persistSettings(prefix string, showPrefix bool, includeBare bool, reasoningReplay string) error {
	cfgFile := findConfigFile()
	if cfgFile == "" {
		return fmt.Errorf("config.yaml not found")
	}
	content, errRead := os.ReadFile(cfgFile)
	if errRead != nil {
		return fmt.Errorf("read config.yaml failed: %w", errRead)
	}
	updated, errUpdate := updateYamlSettings(content, prefix, showPrefix, includeBare, reasoningReplay)
	if errUpdate != nil {
		return fmt.Errorf("update config.yaml failed: %w", errUpdate)
	}
	if errWrite := os.WriteFile(cfgFile, updated, 0600); errWrite != nil {
		return fmt.Errorf("write config.yaml failed: %w", errWrite)
	}
	return nil
}

func updateYamlSettings(content []byte, prefix string, showPrefix bool, includeBare bool, reasoningReplay string) ([]byte, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, err
	}
	if len(root.Content) == 0 {
		return nil, fmt.Errorf("empty yaml")
	}
	doc := root.Content[0]

	var pluginsNode *yaml.Node
	for i := 0; i < len(doc.Content); i += 2 {
		if doc.Content[i].Value == "plugins" {
			pluginsNode = doc.Content[i+1]
			break
		}
	}
	if pluginsNode == nil {
		return nil, fmt.Errorf("plugins node not found")
	}

	var configsNode *yaml.Node
	for i := 0; i < len(pluginsNode.Content); i += 2 {
		if pluginsNode.Content[i].Value == "configs" {
			configsNode = pluginsNode.Content[i+1]
			break
		}
	}
	if configsNode == nil {
		return nil, fmt.Errorf("configs node not found")
	}

	var ccNode *yaml.Node
	for i := 0; i < len(configsNode.Content); i += 2 {
		if configsNode.Content[i].Value == "commandcode-go" || configsNode.Content[i].Value == "cmdc" {
			ccNode = configsNode.Content[i+1]
			break
		}
	}
	if ccNode == nil {
		return nil, fmt.Errorf("commandcode-go node not found")
	}

	setScalar := func(key, val string) {
		for i := 0; i < len(ccNode.Content); i += 2 {
			if ccNode.Content[i].Value == key {
				ccNode.Content[i+1].Value = val
				return
			}
		}
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key}
		valNode := &yaml.Node{Kind: yaml.ScalarNode, Value: val}
		ccNode.Content = append(ccNode.Content, keyNode, valNode)
	}

	setScalar("prefix", prefix)
	setScalar("show_prefix", strconv.FormatBool(showPrefix))
	setScalar("include_bare_models", strconv.FormatBool(includeBare))
	setScalar("reasoning_replay", reasoningReplay)

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}
