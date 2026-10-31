package check

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type localProviderConfig struct {
	ID        string   `json:"id"`
	Preset    string   `json:"preset"`
	Models    []string `json:"models"`
	Delisted  []string `json:"delisted"`
	Off       bool     `json:"off"`
	Hidden    bool     `json:"hidden"`
	NoModels  bool     `json:"noModels"`
	Chat      string   `json:"chat"`
	Responses string   `json:"responses"`
	Anthropic string   `json:"anthropic"`
	Gemini    string   `json:"gemini"`
	Decide    string   `json:"decide"`
}

func (p localProviderConfig) active() bool {
	if p.ID == "" || p.ID == "group" || p.ID == "auto" || p.Off || p.Hidden {
		return false
	}
	if p.NoModels && len(p.Models) == 0 {
		return false
	}
	if len(p.Models) == 0 && len(p.Delisted) > 0 {
		return false
	}
	if p.Decide != "" && p.Chat == "" && p.Responses == "" && p.Anthropic == "" && p.Gemini == "" {
		return false
	}
	return true
}

// listModels reads the local Magpie internal provider catalog (<provider>/<model>)
// without querying the external /v1/models endpoint or exposing routing group aliases.
func (c *groupController) listModels(ctx context.Context) ([]GatewayModel, error) {
	if strings.TrimSpace(c.configDir) == "" {
		return nil, errors.New("未配置本机 Magpie 配置目录")
	}
	raw, err := os.ReadFile(filepath.Join(c.configDir, "providers.json"))
	if err != nil {
		return nil, errors.New("无法读取本机 Magpie providers.json")
	}
	var config struct {
		Providers []localProviderConfig `json:"providers"`
		Groups    []struct {
			Members []string `json:"members"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, errors.New("本机 Magpie providers.json 格式无效")
	}

	activeProviders := make(map[string]bool, len(config.Providers))
	inactiveProviders := map[string]bool{"group": true, "auto": true}
	for _, p := range config.Providers {
		if p.active() {
			activeProviders[p.ID] = true
		} else if p.ID != "" {
			inactiveProviders[p.ID] = true
		}
	}

	cacheDirs := c.localCacheDirs()
	stateModels, stateProviders := readCachedProviderStateModels(cacheDirs)
	for _, id := range stateProviders {
		if !inactiveProviders[id] {
			activeProviders[id] = true
		}
	}

	models := []GatewayModel{}
	seen := make(map[string]bool)
	appendModel := func(group, model string) {
		group = strings.TrimSpace(group)
		model = strings.TrimSpace(model)
		if group == "" || model == "" || group == "group" || group == "auto" || inactiveProviders[group] {
			return
		}
		id := group + "/" + model
		if seen[id] {
			return
		}
		seen[id] = true
		models = append(models, GatewayModel{ID: id, Group: group})
	}

	for _, p := range config.Providers {
		if !p.active() {
			continue
		}
		picked := p.Models
		if len(picked) == 0 {
			picked = readCachedProviderModels(cacheDirs, p, stateModels[p.ID])
		}
		for _, model := range picked {
			appendModel(p.ID, model)
		}
	}
	for _, id := range stateProviders {
		if inactiveProviders[id] {
			continue
		}
		for _, model := range stateModels[id] {
			appendModel(id, model)
		}
	}

	var cliErr error
	if c.magpiePath != "" {
		if env, ok := c.cliEnvForConfig(); ok {
			cliCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			cmd := exec.CommandContext(cliCtx, c.magpiePath, "models")
			cmd.Env = setEnvKV(env, "NO_COLOR", "1")
			cmd.WaitDelay = time.Second
			out, err := cmd.Output()
			cancel()
			if err != nil {
				cliErr = err
			} else {
				for _, m := range parseLocalModels(string(out), activeProviders) {
					appendModel(m.Group, strings.TrimPrefix(m.ID, m.Group+"/"))
				}
			}
		}
	}

	for _, g := range config.Groups {
		for _, member := range g.Members {
			clean := strings.TrimSuffix(strings.TrimSpace(member), ":fast")
			clean, _ = splitMemberEffort(clean)
			group, model, ok := strings.Cut(clean, "/")
			if ok && activeProviders[group] {
				appendModel(group, model)
			}
		}
	}

	if len(models) == 0 && cliErr != nil {
		return nil, errors.New("读取本机 Magpie 模型目录失败，请检查 magpie 命令和配置目录")
	}
	return models, nil
}

func (c *groupController) localCacheDirs() []string {
	dir := filepath.Clean(c.configDir)
	candidates := []string{
		filepath.Join(dir, "cache"),
	}
	if filepath.Base(dir) == "magpie" {
		parent := filepath.Dir(dir)
		root := filepath.Dir(parent)
		if cur, ok := os.LookupEnv("XDG_CONFIG_HOME"); ok && cur != "" && filepath.Clean(filepath.Join(cur, "magpie")) == dir {
			if cacheHome := strings.TrimSpace(os.Getenv("XDG_CACHE_HOME")); cacheHome != "" {
				candidates = append(candidates, filepath.Join(filepath.Clean(cacheHome), "magpie"))
			}
		}
		candidates = append(candidates,
			filepath.Join(parent, "cache", "magpie"),
			filepath.Join(root, ".cache", "magpie"),
			filepath.Join(root, "cache", "magpie"),
		)
	}
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, cand := range candidates {
		cand = filepath.Clean(cand)
		if cand == "" || seen[cand] {
			continue
		}
		seen[cand] = true
		out = append(out, cand)
	}
	return out
}

func readCachedProviderModels(cacheDirs []string, p localProviderConfig, stateFallback []string) []string {
	for _, dir := range cacheDirs {
		raw, err := os.ReadFile(filepath.Join(dir, "models", p.ID+".json"))
		if err != nil {
			continue
		}
		var cached struct {
			Models []struct {
				ID      string `json:"ID"`
				Draws   bool   `json:"Draws"`
				Films   bool   `json:"Films"`
				Decides bool   `json:"Decides"`
			} `json:"models"`
		}
		if err := json.Unmarshal(raw, &cached); err != nil {
			continue
		}
		var ids []string
		for _, m := range cached.Models {
			if m.Draws || m.Films || m.Decides || strings.TrimSpace(m.ID) == "" {
				continue
			}
			ids = append(ids, m.ID)
		}
		if p.Preset != "remote-magpie" && len(ids) > 24 {
			ids = ids[:24]
		}
		if len(ids) > 0 {
			return ids
		}
	}
	return stateFallback
}

func readCachedProviderStateModels(cacheDirs []string) (map[string][]string, []string) {
	out := make(map[string][]string)
	var order []string
	seenProvider := make(map[string]bool)
	for _, dir := range cacheDirs {
		raw, err := os.ReadFile(filepath.Join(dir, "providers-state.json"))
		if err != nil {
			continue
		}
		var state struct {
			Providers struct {
				Providers []struct {
					ID     string `json:"id"`
					Off    bool   `json:"off"`
					Models []struct {
						ID    string `json:"id"`
						On    bool   `json:"on"`
						NoAPI bool   `json:"noAPI"`
						Kind  string `json:"kind"`
					} `json:"models"`
				} `json:"providers"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			continue
		}
		for _, p := range state.Providers.Providers {
			if p.ID == "" || p.Off || p.ID == "group" || p.ID == "auto" {
				continue
			}
			if !seenProvider[p.ID] {
				seenProvider[p.ID] = true
				order = append(order, p.ID)
			}
			if len(out[p.ID]) > 0 {
				continue
			}
			for _, m := range p.Models {
				if m.On && !m.NoAPI && m.Kind == "" && strings.TrimSpace(m.ID) != "" {
					out[p.ID] = append(out[p.ID], m.ID)
				}
			}
		}
	}
	return out, order
}

// parseLocalModels keeps the CLI's padded ID column for active internal providers,
// excluding external routing groups (group/*, auto/*), descriptions, and URLs.
func parseLocalModels(out string, providers map[string]bool) []GatewayModel {
	models := []GatewayModel{}
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "    ") {
			continue
		}
		id := reTwoPlusSpaces.Split(strings.TrimSpace(line), 2)[0]
		group, model, ok := strings.Cut(id, "/")
		if !ok || model == "" || group == "group" || group == "auto" || !providers[group] || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, GatewayModel{ID: id, Group: group})
	}
	return models
}
