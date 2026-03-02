package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const defaultPinchTabTimeout = 20 * time.Second
const maxPinchTabLogSnippet = 240

type PinchTabTool struct {
	baseURL string
	token   string
	client  *http.Client
}

type pinchTabParams struct {
	Operation string `json:"operation"`
	URL       string `json:"url,omitempty"`
	TabID     string `json:"tab_id,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Ref       string `json:"ref,omitempty"`
	Value     string `json:"value,omitempty"`
	Script    string `json:"script,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Filter    string `json:"filter,omitempty"`
}

type pinchSnapshotResponse struct {
	Nodes []pinchSnapshotNode `json:"nodes"`
}

type pinchSnapshotNode struct {
	Ref  string `json:"ref"`
	Role string `json:"role"`
	Name string `json:"name"`
}

var pinchRefPattern = regexp.MustCompile(`^e\d+$`)
var pinchTokenPattern = regexp.MustCompile(`[a-z0-9]+`)

func NewPinchTabTool(baseURL string, token string) Tool {
	return &PinchTabTool{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client:  &http.Client{Timeout: defaultPinchTabTimeout},
	}
}

func (t *PinchTabTool) GetName() string {
	return "pinchtab"
}

func (t *PinchTabTool) GetDescription() string {
	return "Browser automation and scraping through PinchTab. Typical sequence: navigate(url) -> snapshot(filter=interactive) -> action(kind,ref[,value]) -> text(). For action, always use a valid ref from snapshot."
}

func (t *PinchTabTool) GetFunction() func(params string) string {
	return func(params string) string {
		if t.baseURL == "" {
			return errorJSON("pinchtab base url is not configured")
		}
		log.Infof("[tool:pinchtab] call received params=%s", truncateForLog(params, maxPinchTabLogSnippet))

		var in pinchTabParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			log.Warnf("[tool:pinchtab] invalid params: %v", err)
			return errorJSON("invalid params: " + err.Error())
		}
		in.Operation = strings.ToLower(strings.TrimSpace(in.Operation))
		if in.Operation == "" {
			log.Warnf("[tool:pinchtab] operation missing")
			return errorJSON("operation is required")
		}

		var out string
		switch in.Operation {
		case "navigate":
			out = t.navigate(in)
		case "action":
			out = t.action(in)
		case "evaluate":
			out = t.evaluate(in)
		case "text":
			out = t.text(in)
		case "snapshot":
			out = t.snapshot(in)
		case "scrape":
			out = t.scrape(in)
		default:
			return errorJSON("unsupported operation: " + in.Operation)
		}

		if isErrorJSON(out) {
			log.Warnf("[tool:pinchtab] operation=%s failed response=%s", in.Operation, truncateForLog(out, maxPinchTabLogSnippet))
		} else {
			log.Infof("[tool:pinchtab] operation=%s succeeded response=%s", in.Operation, truncateForLog(out, maxPinchTabLogSnippet))
		}
		return out
	}
}

func (t *PinchTabTool) IsImmediate() bool {
	return false
}

func (t *PinchTabTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "PinchTab operation to execute.",
				"enum":        []string{"navigate", "action", "evaluate", "text", "snapshot", "scrape"},
			},
			"url": map[string]any{
				"type":        "string",
				"description": "URL for navigate/scrape.",
			},
			"tab_id": map[string]any{
				"type":        "string",
				"description": "Optional PinchTab tabId to target current tab/session.",
			},
			"kind": map[string]any{
				"type":        "string",
				"description": "Action kind for operation=action. Examples: click, type, fill, press, hover, select. Usually requires ref from snapshot.",
			},
			"ref": map[string]any{
				"type":        "string",
				"description": "Element reference from snapshot nodes (e.g. e5). Required for most action kinds.",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "Input payload for operation=action. Mapped as text for kind=type/fill and key for kind=press.",
			},
			"script": map[string]any{
				"type":        "string",
				"description": "JavaScript source for operation=evaluate.",
			},
			"mode": map[string]any{
				"type":        "string",
				"description": "Optional mode for operation=text or scrape. Example: readability or raw.",
			},
			"filter": map[string]any{
				"type":        "string",
				"description": "Optional snapshot filter (e.g. interactive).",
			},
		},
		Required: []string{"operation"},
	}
}

func (t *PinchTabTool) navigate(in pinchTabParams) string {
	target := strings.TrimSpace(in.URL)
	if target == "" {
		return errorJSON("url is required for navigate")
	}

	body := map[string]any{"url": target}
	if strings.TrimSpace(in.TabID) != "" {
		body["tabId"] = strings.TrimSpace(in.TabID)
	}
	return t.doJSON(http.MethodPost, "/navigate", body)
}

func (t *PinchTabTool) action(in pinchTabParams) string {
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		return errorJSON("kind is required for action")
	}
	kind = strings.ToLower(kind)
	ref := strings.TrimSpace(in.Ref)
	value := strings.TrimSpace(in.Value)

	switch kind {
	case "click", "hover", "focus", "check", "uncheck":
		if ref == "" {
			return errorJSON("ref is required for action kind=" + kind + ". call snapshot first and use one of its refs (for example e12)")
		}
	case "type", "fill", "select":
		if ref == "" {
			return errorJSON("ref is required for action kind=" + kind + ". call snapshot first and use one of its refs (for example e12)")
		}
		if value == "" {
			return errorJSON("value is required for action kind=" + kind)
		}
	case "press":
		if ref == "" {
			return errorJSON("ref is required for action kind=" + kind + ". call snapshot first and use one of its refs (for example e12)")
		}
	}
	if ref != "" && !pinchRefPattern.MatchString(ref) {
		resolvedRef, resolveErr := t.resolveRef(ref, strings.TrimSpace(in.TabID))
		if resolveErr != nil {
			log.Warnf("[tool:pinchtab] selector-like ref could not be resolved ref=%q err=%v", ref, resolveErr)
			return errorJSON("ref must be a snapshot ref like e12. selector-style refs are not supported directly; call snapshot first and use one of its refs")
		}
		log.Infof("[tool:pinchtab] resolved selector-like ref=%q -> %q", ref, resolvedRef)
		ref = resolvedRef
	}

	body := map[string]any{"kind": kind}
	if ref != "" {
		body["ref"] = ref
	}
	if value != "" {
		switch kind {
		case "type", "fill":
			body["text"] = value
		case "press":
			if value == "" {
				value = "Enter"
			}
			body["key"] = value
		default:
			body["value"] = value
		}
	} else if kind == "press" {
		body["key"] = "Enter"
	}
	if strings.TrimSpace(in.TabID) != "" {
		body["tabId"] = strings.TrimSpace(in.TabID)
	}
	return t.doJSON(http.MethodPost, "/action", body)
}

func (t *PinchTabTool) evaluate(in pinchTabParams) string {
	script := strings.TrimSpace(in.Script)
	if script == "" {
		return errorJSON("script is required for evaluate")
	}

	body := map[string]any{"script": script}
	if strings.TrimSpace(in.TabID) != "" {
		body["tabId"] = strings.TrimSpace(in.TabID)
	}
	return t.doJSON(http.MethodPost, "/evaluate", body)
}

func (t *PinchTabTool) text(in pinchTabParams) string {
	path := "/text"
	q := url.Values{}
	if strings.TrimSpace(in.TabID) != "" {
		q.Set("tabId", strings.TrimSpace(in.TabID))
	}
	if m := strings.TrimSpace(in.Mode); m != "" {
		q.Set("mode", m)
	}
	if qs := q.Encode(); qs != "" {
		path += "?" + qs
	}
	return t.doRaw(http.MethodGet, path, nil)
}

func (t *PinchTabTool) snapshot(in pinchTabParams) string {
	path := "/snapshot"
	q := url.Values{}
	if strings.TrimSpace(in.TabID) != "" {
		q.Set("tabId", strings.TrimSpace(in.TabID))
	}
	if f := strings.TrimSpace(in.Filter); f != "" {
		q.Set("filter", f)
	}
	if qs := q.Encode(); qs != "" {
		path += "?" + qs
	}
	return t.doRaw(http.MethodGet, path, nil)
}

func (t *PinchTabTool) scrape(in pinchTabParams) string {
	target := strings.TrimSpace(in.URL)
	if target == "" {
		return errorJSON("url is required for scrape")
	}

	nav := map[string]any{"url": target}
	if strings.TrimSpace(in.TabID) != "" {
		nav["tabId"] = strings.TrimSpace(in.TabID)
	}
	navResp := t.doJSON(http.MethodPost, "/navigate", nav)
	if isErrorJSON(navResp) {
		return navResp
	}

	path := "/text"
	q := url.Values{}
	if m := strings.TrimSpace(in.Mode); m != "" {
		q.Set("mode", m)
	}
	if strings.TrimSpace(in.TabID) != "" {
		q.Set("tabId", strings.TrimSpace(in.TabID))
	}
	if qs := q.Encode(); qs != "" {
		path += "?" + qs
	}
	textResp := t.doRaw(http.MethodGet, path, nil)
	if isErrorJSON(textResp) {
		return textResp
	}

	payload := map[string]any{
		"navigate": json.RawMessage(navResp),
		"text":     parseJSONOrString(textResp),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errorJSON("serialize scrape response failed: " + err.Error())
	}
	return string(data)
}

func (t *PinchTabTool) doJSON(method string, path string, body any) string {
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return errorJSON("serialize request failed: " + err.Error())
		}
		payload = data
	}
	return t.doRaw(method, path, payload)
}

func (t *PinchTabTool) doRaw(method string, path string, body []byte) string {
	target := t.baseURL + path
	startedAt := time.Now()
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	log.Infof("[tool:pinchtab] request method=%s path=%s body=%s", method, path, truncateForLog(string(body), maxPinchTabLogSnippet))

	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		log.Warnf("[tool:pinchtab] build request failed: %v", err)
		return errorJSON("build request failed: " + err.Error())
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.token != "" {
		req.Header.Set("Authorization", "Bearer "+t.token)
		req.Header.Set("X-Bridge-Token", t.token)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		log.Warnf("[tool:pinchtab] request failed method=%s path=%s err=%v", method, path, err)
		return errorJSON("pinchtab request failed: " + err.Error())
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Warnf("[tool:pinchtab] read response failed method=%s path=%s err=%v", method, path, err)
		return errorJSON("read pinchtab response failed: " + err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Warnf("[tool:pinchtab] response status=%d method=%s path=%s body=%s elapsed=%s", resp.StatusCode, method, path, truncateForLog(string(data), maxPinchTabLogSnippet), time.Since(startedAt).Round(time.Millisecond))
		return errorJSON(fmt.Sprintf("pinchtab error (%d): %s", resp.StatusCode, strings.TrimSpace(string(data))))
	}
	log.Infof("[tool:pinchtab] response status=%d method=%s path=%s bytes=%d elapsed=%s", resp.StatusCode, method, path, len(data), time.Since(startedAt).Round(time.Millisecond))

	return strings.TrimSpace(string(data))
}

func truncateForLog(v string, max int) string {
	s := strings.TrimSpace(v)
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

func parseJSONOrString(v string) any {
	var out any
	if err := json.Unmarshal([]byte(v), &out); err == nil {
		return out
	}
	return v
}

func isErrorJSON(v string) bool {
	var obj map[string]any
	if err := json.Unmarshal([]byte(v), &obj); err != nil {
		return false
	}
	_, ok := obj["error"]
	return ok
}

func (t *PinchTabTool) resolveRef(selectorLikeRef string, tabID string) (string, error) {
	path := "/snapshot"
	q := url.Values{}
	q.Set("filter", "interactive")
	if tabID != "" {
		q.Set("tabId", tabID)
	}
	if qs := q.Encode(); qs != "" {
		path += "?" + qs
	}
	raw := t.doRaw(http.MethodGet, path, nil)
	if isErrorJSON(raw) {
		return "", fmt.Errorf("snapshot failed: %s", raw)
	}

	var snap pinchSnapshotResponse
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return "", fmt.Errorf("decode snapshot failed: %w", err)
	}
	if len(snap.Nodes) == 0 {
		return "", fmt.Errorf("snapshot returned no nodes")
	}

	ref := bestNodeRef(selectorLikeRef, snap.Nodes)
	if ref == "" {
		return "", fmt.Errorf("no candidate node matched")
	}
	return ref, nil
}

func bestNodeRef(selectorLikeRef string, nodes []pinchSnapshotNode) string {
	query := strings.ToLower(strings.TrimSpace(selectorLikeRef))
	tokens := pinchTokenPattern.FindAllString(query, -1)
	stop := map[string]struct{}{
		"input": {}, "button": {}, "div": {}, "span": {}, "text": {}, "type": {},
		"aria": {}, "label": {}, "name": {}, "id": {}, "class": {}, "placeholder": {},
	}

	type scored struct {
		ref   string
		score int
	}
	candidates := make([]scored, 0, len(nodes))
	for _, n := range nodes {
		if !pinchRefPattern.MatchString(strings.TrimSpace(n.Ref)) {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(n.Role))
		name := strings.ToLower(strings.TrimSpace(n.Name))
		score := 0

		if strings.Contains(query, "search") || strings.Contains(query, "cerca") {
			if strings.Contains(role, "search") || strings.Contains(name, "search") || strings.Contains(name, "cerca") {
				score += 10
			}
		}
		if strings.Contains(role, "textbox") || strings.Contains(role, "searchbox") || strings.Contains(role, "combobox") {
			score += 4
		}

		for _, tok := range tokens {
			if _, skip := stop[tok]; skip || len(tok) < 2 {
				continue
			}
			if strings.Contains(name, tok) {
				score += 3
			}
			if strings.Contains(role, tok) {
				score += 2
			}
		}
		if score > 0 {
			candidates = append(candidates, scored{ref: n.Ref, score: score})
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})
	return candidates[0].ref
}
