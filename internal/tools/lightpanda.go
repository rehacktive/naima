package tools

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"

	playwright "github.com/playwright-community/playwright-go"
)

const (
	defaultLightpandaTimeoutMS = 30000
	defaultLightpandaWaitMS    = 500
)

type LightpandaTool struct {
	endpoint  string
	timeoutMS int
	mu        sync.Mutex
	pw        *playwright.Playwright
	browser   playwright.Browser
	context   playwright.BrowserContext
	page      playwright.Page
	current   string
}

type lightpandaParams struct {
	Operation string `json:"operation"`
	URL       string `json:"url,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Value     string `json:"value,omitempty"`
	Script    string `json:"script,omitempty"`
	WaitMS    int    `json:"wait_ms,omitempty"`
	FullPage  bool   `json:"full_page,omitempty"`
}

func NewLightpandaTool(endpoint string, timeoutMS int) Tool {
	if timeoutMS <= 0 {
		timeoutMS = defaultLightpandaTimeoutMS
	}
	return &LightpandaTool{
		endpoint:  strings.TrimSpace(endpoint),
		timeoutMS: timeoutMS,
	}
}

func (t *LightpandaTool) GetName() string {
	return "lightpanda"
}

func (t *LightpandaTool) GetDescription() string {
	return "Browser automation through a dockerized Lightpanda CDP instance. Supports: goto, scrape, click, type, press, evaluate, screenshot, close."
}

func (t *LightpandaTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in lightpandaParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		op := strings.ToLower(strings.TrimSpace(in.Operation))
		if op == "" {
			return errorJSON("operation is required")
		}
		waitMS := max(in.WaitMS, 0)
		if waitMS == 0 {
			waitMS = defaultLightpandaWaitMS
		}

		return t.withPage(op, strings.TrimSpace(in.URL), func(page playwright.Page, currentURL string) (string, error) {
			switch op {
			case "close", "reset":
				if err := t.closeSession(); err != nil {
					return "", errTool("close failed: " + err.Error())
				}
				return `{"status":"closed"}`, nil
			case "goto", "navigate":
				return t.scrape(page)
			case "scrape":
				return t.scrape(page)
			case "click":
				sel := strings.TrimSpace(in.Selector)
				if sel == "" {
					return "", errTool("selector is required for click")
				}
				if err := page.Click(sel); err != nil {
					return "", errTool("click failed: " + err.Error())
				}
				page.WaitForTimeout(float64(waitMS))
				return t.scrape(page)
			case "type":
				sel := strings.TrimSpace(in.Selector)
				if sel == "" {
					return "", errTool("selector is required for type")
				}
				val := in.Value
				if strings.TrimSpace(val) == "" {
					return "", errTool("value is required for type")
				}
				if err := page.Fill(sel, val); err != nil {
					return "", errTool("type failed: " + err.Error())
				}
				page.WaitForTimeout(float64(waitMS))
				return t.scrape(page)
			case "press":
				sel := strings.TrimSpace(in.Selector)
				if sel == "" {
					return "", errTool("selector is required for press")
				}
				key := strings.TrimSpace(in.Value)
				if key == "" {
					key = "Enter"
				}
				if err := page.Press(sel, key); err != nil {
					return "", errTool("press failed: " + err.Error())
				}
				page.WaitForTimeout(float64(waitMS))
				return t.scrape(page)
			case "evaluate":
				script := strings.TrimSpace(in.Script)
				if script == "" {
					return "", errTool("script is required for evaluate")
				}
				val, err := page.Evaluate(script)
				if err != nil {
					return "", errTool("evaluate failed: " + normalizeEvaluateError(err.Error()))
				}
				payload := map[string]any{"url": currentURL, "value": val}
				out, mErr := json.Marshal(payload)
				if mErr != nil {
					return "", errTool("serialize evaluate result failed: " + mErr.Error())
				}
				return string(out), nil
			case "screenshot":
				img, err := page.Screenshot(playwright.PageScreenshotOptions{FullPage: playwright.Bool(in.FullPage)})
				if err != nil {
					return "", errTool("screenshot failed: " + err.Error())
				}
				payload := map[string]any{
					"url":         currentURL,
					"contentType": "image/png",
					"base64":      base64.StdEncoding.EncodeToString(img),
				}
				out, mErr := json.Marshal(payload)
				if mErr != nil {
					return "", errTool("serialize screenshot failed: " + mErr.Error())
				}
				return string(out), nil
			default:
				return "", errTool("unsupported operation: " + op)
			}
		})
	}
}

func (t *LightpandaTool) IsImmediate() bool {
	return false
}

func (t *LightpandaTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "Operation: goto/navigate/scrape, click, type, press, evaluate, screenshot, close.",
				"enum":        []string{"goto", "navigate", "scrape", "click", "type", "press", "evaluate", "screenshot", "close", "reset"},
			},
			"url": map[string]any{
				"type":        "string",
				"description": "Page URL to open. Required for goto/navigate and first use; optional for follow-up actions.",
			},
			"selector": map[string]any{
				"type":        "string",
				"description": "CSS selector for click/type/press.",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "Text for type, key for press (default Enter).",
			},
			"script": map[string]any{
				"type":        "string",
				"description": "JavaScript expression for evaluate.",
			},
			"wait_ms": map[string]any{
				"type":        "integer",
				"description": "Optional wait after action before scraping.",
				"minimum":     0,
			},
			"full_page": map[string]any{
				"type":        "boolean",
				"description": "For screenshot operation, capture full page.",
			},
		},
		Required: []string{"operation"},
	}
}

func (t *LightpandaTool) withPage(op string, targetURL string, fn func(page playwright.Page, currentURL string) (string, error)) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	if op == "close" || op == "reset" {
		out, runErr := fn(nil, "")
		if runErr != nil {
			return errorJSON(runErr.Error())
		}
		return out
	}

	page, currentURL, err := t.ensurePageFor(op, targetURL)
	if err != nil {
		return errorJSON(err.Error())
	}

	out, runErr := fn(page, currentURL)
	if runErr != nil {
		return errorJSON(runErr.Error())
	}
	return out
}

func (t *LightpandaTool) ensurePageFor(op string, targetURL string) (playwright.Page, string, error) {
	needsNavigation := op == "goto" || op == "navigate"
	if t.page == nil {
		if err := t.startSession(); err != nil {
			return nil, "", errTool("lightpanda start failed: " + err.Error())
		}
	}

	if needsNavigation || targetURL != "" {
		if strings.TrimSpace(targetURL) == "" {
			return nil, "", errTool("url is required for operation " + op)
		}
		if _, err := t.page.Goto(targetURL, playwright.PageGotoOptions{Timeout: playwright.Float(float64(t.timeoutMS))}); err != nil {
			return nil, "", errTool("navigate failed: " + err.Error())
		}
		t.page.WaitForTimeout(250)
		t.current = targetURL
	}

	if t.current == "" {
		return nil, "", errTool("url is required for first call; use operation=goto with a url")
	}

	t.page.SetDefaultTimeout(float64(t.timeoutMS))
	return t.page, t.current, nil
}

func (t *LightpandaTool) startSession() error {
	if strings.TrimSpace(t.endpoint) == "" {
		return errTool("lightpanda endpoint is not configured")
	}
	if err := ensurePlaywrightInstalled(); err != nil {
		return err
	}
	pw, err := playwright.Run()
	if err != nil {
		return err
	}
	browser, err := pw.Chromium.ConnectOverCDP(t.endpoint, playwright.BrowserTypeConnectOverCDPOptions{
		Timeout: playwright.Float(float64(t.timeoutMS)),
	})
	if err != nil {
		_ = pw.Stop()
		return err
	}
	var ctx playwright.BrowserContext
	if contexts := browser.Contexts(); len(contexts) > 0 {
		ctx = contexts[0]
	} else {
		ctx, err = browser.NewContext()
		if err != nil {
			_ = browser.Close()
			_ = pw.Stop()
			return err
		}
	}
	page, err := ctx.NewPage()
	if err != nil {
		_ = ctx.Close()
		_ = browser.Close()
		_ = pw.Stop()
		return err
	}
	t.pw = pw
	t.browser = browser
	t.context = ctx
	t.page = page
	t.current = ""
	return nil
}

func (t *LightpandaTool) closeSession() error {
	var firstErr error
	if t.page != nil {
		if err := t.page.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if t.context != nil {
		if err := t.context.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if t.browser != nil {
		if err := t.browser.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if t.pw != nil {
		if err := t.pw.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	t.page = nil
	t.context = nil
	t.browser = nil
	t.pw = nil
	t.current = ""
	return firstErr
}

func (t *LightpandaTool) scrape(page playwright.Page) (string, error) {
	title, _ := page.Title()
	text, err := page.InnerText("body")
	if err != nil {
		text = ""
	}
	if len(text) > 12000 {
		text = text[:12000] + "...(truncated)"
	}
	payload := map[string]any{
		"title": title,
		"text":  strings.TrimSpace(text),
		"at":    time.Now().UTC().Format(time.RFC3339),
	}
	out, mErr := json.Marshal(payload)
	if mErr != nil {
		return "", errTool("serialize scrape result failed: " + mErr.Error())
	}
	return string(out), nil
}
