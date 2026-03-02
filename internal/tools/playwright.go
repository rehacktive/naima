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
	defaultPlaywrightTimeoutMS = 30000
	defaultPlaywrightWaitMS    = 500
)

type PlaywrightTool struct {
	headless  bool
	timeoutMS int
	mu        sync.Mutex
	pw        *playwright.Playwright
	browser   playwright.Browser
	page      playwright.Page
	current   string
}

type playwrightParams struct {
	Operation string `json:"operation"`
	URL       string `json:"url,omitempty"`
	Selector  string `json:"selector,omitempty"`
	Value     string `json:"value,omitempty"`
	Script    string `json:"script,omitempty"`
	WaitMS    int    `json:"wait_ms,omitempty"`
	FullPage  bool   `json:"full_page,omitempty"`
}

var playwrightInstallOnce sync.Once
var playwrightInstallErr error

func NewPlaywrightTool(headless bool, timeoutMS int) Tool {
	if timeoutMS <= 0 {
		timeoutMS = defaultPlaywrightTimeoutMS
	}
	return &PlaywrightTool{
		headless:  headless,
		timeoutMS: timeoutMS,
	}
}

func (t *PlaywrightTool) GetName() string {
	return "playwright"
}

func (t *PlaywrightTool) GetDescription() string {
	return "Browser automation with Playwright Go. Supports: scrape, click, type, press, evaluate, screenshot."
}

func (t *PlaywrightTool) GetFunction() func(params string) string {
	return func(params string) string {
		var in playwrightParams
		if err := json.Unmarshal([]byte(params), &in); err != nil {
			return errorJSON("invalid params: " + err.Error())
		}

		op := strings.ToLower(strings.TrimSpace(in.Operation))
		if op == "" {
			return errorJSON("operation is required")
		}

		waitMS := in.WaitMS
		if waitMS < 0 {
			waitMS = 0
		}
		if waitMS == 0 {
			waitMS = defaultPlaywrightWaitMS
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
				payload := map[string]any{
					"url":   currentURL,
					"value": val,
				}
				out, mErr := json.Marshal(payload)
				if mErr != nil {
					return "", errTool("serialize evaluate result failed: " + mErr.Error())
				}
				return string(out), nil
			case "screenshot":
				img, err := page.Screenshot(playwright.PageScreenshotOptions{
					FullPage: playwright.Bool(in.FullPage),
				})
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

func (t *PlaywrightTool) IsImmediate() bool {
	return false
}

func (t *PlaywrightTool) GetParameters() Parameters {
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

func (t *PlaywrightTool) withPage(op string, targetURL string, fn func(page playwright.Page, currentURL string) (string, error)) string {
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

func (t *PlaywrightTool) ensurePageFor(op string, targetURL string) (playwright.Page, string, error) {
	needsNavigation := op == "goto" || op == "navigate"
	if t.page == nil {
		if err := t.startSession(); err != nil {
			return nil, "", errTool("playwright start failed: " + err.Error())
		}
	}

	if needsNavigation || targetURL != "" {
		if strings.TrimSpace(targetURL) == "" {
			return nil, "", errTool("url is required for operation " + op)
		}
		if _, err := t.page.Goto(targetURL, playwright.PageGotoOptions{
			Timeout: playwright.Float(float64(t.timeoutMS)),
		}); err != nil {
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

func (t *PlaywrightTool) startSession() error {
	if err := ensurePlaywrightInstalled(); err != nil {
		return err
	}
	pw, err := playwright.Run()
	if err != nil {
		return err
	}
	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(t.headless),
		Timeout:  playwright.Float(float64(t.timeoutMS)),
	})
	if err != nil {
		_ = pw.Stop()
		return err
	}
	page, err := browser.NewPage()
	if err != nil {
		_ = browser.Close()
		_ = pw.Stop()
		return err
	}
	t.pw = pw
	t.browser = browser
	t.page = page
	t.current = ""
	return nil
}

func (t *PlaywrightTool) closeSession() error {
	var firstErr error
	if t.page != nil {
		if err := t.page.Close(); err != nil && firstErr == nil {
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
	t.browser = nil
	t.pw = nil
	t.current = ""
	return firstErr
}

func (t *PlaywrightTool) scrape(page playwright.Page) (string, error) {
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

func ensurePlaywrightInstalled() error {
	playwrightInstallOnce.Do(func() {
		playwrightInstallErr = playwright.Install()
	})
	return playwrightInstallErr
}

func errTool(message string) error {
	return &toolError{message: message}
}

type toolError struct {
	message string
}

func (e *toolError) Error() string {
	return e.message
}

func normalizeEvaluateError(raw string) string {
	msg := strings.TrimSpace(raw)
	l := strings.ToLower(msg)
	if strings.Contains(l, "cannot read properties of null") {
		return "script tried to access an element that does not exist (null). Verify selector first, or prefer playwright operations (click/type/press) instead of direct DOM click in evaluate."
	}
	return msg
}
