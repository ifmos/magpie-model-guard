package check

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ProbeConfig is the gateway connection used for probe requests.
type ProbeConfig struct {
	GatewayURL string `json:"gateway_url"`
	GatewayKey string `json:"gateway_key"`
}

// ValidateGateway checks a gateway base URL shape.
func ValidateGateway(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("请输入有效 Magpie HTTP(S) 网关地址")
	}
	return nil
}

var probeHTTP = &http.Client{
	Timeout: 180 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return errors.New("不允许网关重定向")
	},
}

// GatewayModel is one entry of the gateway model list.
type GatewayModel struct {
	ID    string `json:"id"`
	Group string `json:"group"`
}

// ListModels fetches /v1/models from the gateway.
func ListModels(ctx context.Context, c ProbeConfig) ([]GatewayModel, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ValidateGateway(c.GatewayURL); err != nil {
		return nil, err
	}
	base := gatewayBase(c.GatewayURL)
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.GatewayKey)
	resp, err := probeHTTP.Do(req)
	if err != nil {
		return nil, errors.New("无法读取 Magpie 模型列表")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("模型列表 HTTP %d", resp.StatusCode)
	}
	var data struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&data); err != nil {
		return nil, errors.New("模型列表无效")
	}
	out := []GatewayModel{}
	for _, m := range data.Data {
		group := m.OwnedBy
		if i := strings.Index(m.ID, "/"); i > 0 {
			group = m.ID[:i]
		}
		out = append(out, GatewayModel{ID: m.ID, Group: group})
	}
	return out, nil
}

func gatewayBase(raw string) string {
	base := strings.TrimRight(raw, "/")
	if !strings.HasSuffix(base, "/v1") {
		base += "/v1"
	}
	return base
}

// ProbeAnswer is the outcome of one probe request.
type ProbeAnswer struct {
	Text    string
	Tokens  int
	Finish  string
	Partial func(string) `json:"-"`
}

// Call sends one chat-completions probe. progress may be nil for a
// non-streaming call.
func Call(ctx context.Context, c ProbeConfig, model, prompt string, progress func(string)) (string, int, error) {
	payload := map[string]any{
		"model":      model,
		"stream":     progress != nil,
		"max_tokens": 8192,
		"messages": []any{
			map[string]string{"role": "user", "content": prompt},
		},
	}
	if progress != nil {
		payload["stream_options"] = map[string]bool{"include_usage": true}
	}
	b, _ := json.Marshal(payload)
	base := gatewayBase(c.GatewayURL)
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.GatewayKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := probeHTTP.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", 0, fmt.Errorf("Magpie HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if progress != nil && strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return readAnswerStream(resp.Body, progress)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if err != nil {
		return "", 0, err
	}
	if len(raw) > 4*1024*1024 {
		return "", 0, errors.New("回复超过大小限制")
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, errors.New("无法解析完整回复")
	}
	if len(out.Choices) != 1 {
		return "", 0, errors.New("缺少唯一有效回答")
	}
	ch := out.Choices[0]
	if ch.Finish != "stop" {
		return ch.Message.Content, out.Usage.Total, errors.New("回复未正常结束或被截断")
	}
	if strings.TrimSpace(ch.Message.Content) == "" {
		return "", out.Usage.Total, errors.New("空回答")
	}
	return ch.Message.Content, out.Usage.Total, nil
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Finish string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		Total int `json:"total_tokens"`
	} `json:"usage"`
}

func readAnswerStream(body io.Reader, progress func(string)) (string, int, error) {
	const limit = 4 * 1024 * 1024
	scanner := bufio.NewScanner(io.LimitReader(body, limit+1))
	scanner.Buffer(make([]byte, 4096), limit)
	var answer strings.Builder
	tokens, bytesRead := 0, 0
	finished, done := false, false
	var event []string
	consume := func() error {
		if len(event) == 0 {
			return nil
		}
		data := strings.Join(event, "\n")
		event = nil
		if data == "[DONE]" {
			done = true
			return nil
		}
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
			Usage struct {
				Total int `json:"total_tokens"`
			} `json:"usage"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || (len(chunk.Error) > 0 && string(chunk.Error) != "null") {
			return errors.New("无法解析流式回复")
		}
		if chunk.Usage.Total > 0 {
			tokens = chunk.Usage.Total
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				return errors.New("缺少唯一有效回答")
			}
			answer.WriteString(choice.Delta.Content)
			if choice.Delta.Content != "" {
				progress(answer.String())
			}
			if choice.Finish != nil {
				if *choice.Finish != "stop" {
					return errors.New("回复未正常结束或被截断")
				}
				finished = true
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += len(line) + 1
		if bytesRead > limit {
			return answer.String(), tokens, errors.New("回复超过大小限制")
		}
		if line == "" {
			if err := consume(); err != nil {
				return answer.String(), tokens, err
			}
			if done {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			event = append(event, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return answer.String(), tokens, err
	}
	if err := consume(); err != nil {
		return answer.String(), tokens, err
	}
	if !finished || !done {
		return answer.String(), tokens, errors.New("流式回复连接中断或未完整结束")
	}
	if strings.TrimSpace(answer.String()) == "" {
		return "", tokens, errors.New("空回答")
	}
	return answer.String(), tokens, nil
}

var codeFence = regexp.MustCompile("(?s)```[a-zA-Z0-9_+-]*\\n(.*?)```")

// ExtractFirstCodeFence returns the first fenced code block, if any.
func ExtractFirstCodeFence(answer string) (string, bool) {
	m := codeFence.FindStringSubmatch(answer)
	if m == nil {
		return "", false
	}
	return m[1], true
}
