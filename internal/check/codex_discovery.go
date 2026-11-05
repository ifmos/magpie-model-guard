package check

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// CodexAccountInfo is public metadata from Magpie's logins.json. Credentials
// in that file are never unmarshalled, stored, logged, or returned.
type CodexAccountInfo struct {
	ID     string `json:"id"`
	User   string `json:"user"`
	Plan   string `json:"plan,omitempty"`
	Lapsed string `json:"lapsed,omitempty"`
	On     bool   `json:"on,omitempty"`
	Paused bool   `json:"paused,omitempty"`
	Own    bool   `json:"own,omitempty"`
}

func codexLoginsPath(configDir string) string {
	return filepath.Join(configDir, "logins.json")
}

// discoverCodexAccounts reads only account identity and display metadata from
// the local Magpie sign-ins.
func discoverCodexAccounts(configDir string) ([]CodexAccountInfo, error) {
	if strings.TrimSpace(configDir) == "" {
		return nil, errors.New("未配置 Magpie 配置目录")
	}
	raw, err := os.ReadFile(codexLoginsPath(configDir))
	if errors.Is(err, os.ErrNotExist) {
		return []CodexAccountInfo{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("无法读取本机 Magpie 账号信息：%w", err)
	}
	if len(raw) > 8<<20 {
		return nil, errors.New("Magpie 账号信息超出安全上限")
	}
	var rows []struct {
		ID     string `json:"id"`
		Agent  string `json:"agent"`
		User   string `json:"user"`
		Plan   string `json:"plan"`
		Hidden string `json:"hidden"`
		Lapsed string `json:"lapsed"`
		On     bool   `json:"on"`
		Paused bool   `json:"paused"`
		Own    bool   `json:"own"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, errors.New("Magpie 账号文件格式不正确")
	}
	seen := map[string]bool{}
	out := make([]CodexAccountInfo, 0, len(rows))
	for _, row := range rows {
		if row.Agent != "codex" || strings.TrimSpace(row.Hidden) != "" {
			continue
		}
		user := strings.TrimSpace(row.User)
		if err := validateCodexAccountUser(user); err != nil {
			return nil, err
		}
		id := strings.ToLower(strings.TrimSpace(row.ID))
		if id == "" {
			// Mirrors magpie's loginID fallback so an entry written before the
			// stable id was assigned resolves to the same account later.
			sum := sha256.Sum256([]byte("magpie login\ncodex\n" + strings.ToLower(user)))
			id = hex.EncodeToString(sum[:8])
		}
		if err := validateCodexAccountID(id); err != nil {
			return nil, err
		}
		if seen[id] {
			return nil, fmt.Errorf("Codex 账号出现重复的稳定 ID：%s", id)
		}
		seen[id] = true
		out = append(out, CodexAccountInfo{
			ID: id, User: user, Plan: strings.TrimSpace(row.Plan),
			Lapsed: strings.TrimSpace(row.Lapsed), On: row.On, Paused: row.Paused, Own: row.Own,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if strings.ToLower(out[i].User) != strings.ToLower(out[j].User) {
			return strings.ToLower(out[i].User) < strings.ToLower(out[j].User)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func validateCodexAccountUser(user string) error {
	if user == "" || len(user) > 512 {
		return errors.New("Codex 账号名称无效")
	}
	for _, r := range user {
		if unicode.IsControl(r) || r == '\r' || r == '\n' {
			return errors.New("Codex 账号名称包含控制字符")
		}
	}
	return nil
}

func validateCodexAccountID(id string) error {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, "/\\\r\n\t ") {
		return errors.New("Codex 账号稳定 ID 无效")
	}
	return nil
}

func validateCodexModel(model string) error {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 512 || strings.ContainsAny(model, "\r\n") {
		return errors.New("请输入有效的 Codex 检测模型")
	}
	if !strings.HasPrefix(model, "codex/") || strings.HasPrefix(model, ProbeAliasPrefix) {
		return errors.New("Codex 账号检测模型必须是原生 codex/ 模型 ID")
	}
	return nil
}
