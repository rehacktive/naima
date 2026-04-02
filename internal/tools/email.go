package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"naima/internal/persona"
)

const (
	defaultEmailTimeout        = 30 * time.Second
	defaultEmailListLimit      = 5
	maxEmailListLimit          = 20
	defaultEmailBodyChars      = 4000
	maxEmailBodyChars          = 20000
	defaultEmailWaitTimeout    = 90
	defaultEmailPollSeconds    = 5
	defaultPOP3TLSPort         = 995
	defaultPOP3PlainPort       = 110
	defaultSMTPSTARTTLSPort    = 587
	defaultSMTPImplicitTLSPort = 465
)

var (
	linkHrefPattern = regexp.MustCompile(`(?is)href\s*=\s*["']([^"']+)["']`)
	linkTextPattern = regexp.MustCompile(`https?://[^\s<>"']+`)
	htmlTagPattern  = regexp.MustCompile(`(?s)<[^>]+>`)
	spacePattern    = regexp.MustCompile(`\s+`)
)

type EmailTool struct {
	pop3    emailPOP3Config
	smtp    emailSMTPConfig
	persona PersonaLookup
}

type PersonaLookup interface {
	BestFact(ctx context.Context, key string) (persona.Fact, bool, error)
}

type emailPOP3Config struct {
	Host               string
	Port               int
	Username           string
	Password           string
	UseTLS             bool
	InsecureSkipVerify bool
	Timeout            time.Duration
}

type emailSMTPConfig struct {
	Host               string
	Port               int
	Username           string
	Password           string
	From               string
	UseSTARTTLS        bool
	UseImplicitTLS     bool
	AuthMethod         string
	InsecureSkipVerify bool
	Timeout            time.Duration
}

type emailParams struct {
	Operation       string   `json:"operation"`
	ID              int      `json:"id,omitempty"`
	UIDL            string   `json:"uidl,omitempty"`
	AfterID         int      `json:"after_id,omitempty"`
	FromContains    string   `json:"from_contains,omitempty"`
	SubjectContains string   `json:"subject_contains,omitempty"`
	BodyContains    string   `json:"body_contains,omitempty"`
	LinkContains    string   `json:"link_contains,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	TimeoutSeconds  int      `json:"timeout_seconds,omitempty"`
	PollSeconds     int      `json:"poll_seconds,omitempty"`
	MaxBodyChars    int      `json:"max_body_chars,omitempty"`
	IncludeBody     bool     `json:"include_body,omitempty"`
	To              []string `json:"to,omitempty"`
	Cc              []string `json:"cc,omitempty"`
	Bcc             []string `json:"bcc,omitempty"`
	Subject         string   `json:"subject,omitempty"`
	Text            string   `json:"text,omitempty"`
	HTML            string   `json:"html,omitempty"`
	ReplyTo         string   `json:"reply_to,omitempty"`
}

type emailMessage struct {
	ID         int       `json:"id"`
	UIDL       string    `json:"uidl,omitempty"`
	Subject    string    `json:"subject,omitempty"`
	From       string    `json:"from,omitempty"`
	To         []string  `json:"to,omitempty"`
	Date       string    `json:"date,omitempty"`
	MessageID  string    `json:"message_id,omitempty"`
	TextBody   string    `json:"text_body,omitempty"`
	HTMLBody   string    `json:"html_body,omitempty"`
	Preview    string    `json:"preview,omitempty"`
	Links      []string  `json:"links,omitempty"`
	ReceivedAt time.Time `json:"-"`
}

func NewEmailToolFromEnv(personaStore PersonaLookup) Tool {
	timeout := time.Duration(envIntDefault("NAIMA_EMAIL_TIMEOUT_MS", int(defaultEmailTimeout/time.Millisecond))) * time.Millisecond
	pop3TLS := envBoolDefault("NAIMA_EMAIL_POP3_TLS", true)
	smtpImplicitTLS := envBoolDefault("NAIMA_EMAIL_SMTP_IMPLICIT_TLS", false)
	smtpSTARTTLS := envBoolDefault("NAIMA_EMAIL_SMTP_STARTTLS", !smtpImplicitTLS)

	pop3Port := envIntDefault("NAIMA_EMAIL_POP3_PORT", defaultPOP3PlainPort)
	if pop3TLS {
		pop3Port = envIntDefault("NAIMA_EMAIL_POP3_PORT", defaultPOP3TLSPort)
	}
	smtpPort := envIntDefault("NAIMA_EMAIL_SMTP_PORT", defaultSMTPSTARTTLSPort)
	if smtpImplicitTLS {
		smtpPort = envIntDefault("NAIMA_EMAIL_SMTP_PORT", defaultSMTPImplicitTLSPort)
	}

	return &EmailTool{
		pop3: emailPOP3Config{
			Host:               strings.TrimSpace(os.Getenv("NAIMA_EMAIL_POP3_HOST")),
			Port:               pop3Port,
			Username:           strings.TrimSpace(os.Getenv("NAIMA_EMAIL_POP3_USER")),
			Password:           os.Getenv("NAIMA_EMAIL_POP3_PASS"),
			UseTLS:             pop3TLS,
			InsecureSkipVerify: envBoolDefault("NAIMA_EMAIL_POP3_INSECURE_SKIP_VERIFY", false),
			Timeout:            timeout,
		},
		smtp: emailSMTPConfig{
			Host:               strings.TrimSpace(os.Getenv("NAIMA_EMAIL_SMTP_HOST")),
			Port:               smtpPort,
			Username:           strings.TrimSpace(os.Getenv("NAIMA_EMAIL_SMTP_USER")),
			Password:           os.Getenv("NAIMA_EMAIL_SMTP_PASS"),
			From:               strings.TrimSpace(os.Getenv("NAIMA_EMAIL_FROM")),
			UseSTARTTLS:        smtpSTARTTLS,
			UseImplicitTLS:     smtpImplicitTLS,
			AuthMethod:         normalizeEmailAuthMethod(os.Getenv("NAIMA_EMAIL_SMTP_AUTH_METHOD")),
			InsecureSkipVerify: envBoolDefault("NAIMA_EMAIL_SMTP_INSECURE_SKIP_VERIFY", false),
			Timeout:            timeout,
		},
		persona: personaStore,
	}
}

func (t *EmailTool) GetName() string {
	return "email"
}

func (t *EmailTool) GetDescription() string {
	return "Reads email over POP3 and sends email over SMTP using env-based mailbox credentials. Supports inbox polling for confirmation flows."
}

func (t *EmailTool) GetFunction() func(params string) string {
	return func(params string) string {
		return t.Execute(context.Background(), params)
	}
}

func (t *EmailTool) Execute(ctx context.Context, params string) string {
	var in emailParams
	if err := jsonUnmarshal(params, &in); err != nil {
		return errorJSON("invalid params: " + err.Error())
	}

	switch strings.ToLower(strings.TrimSpace(in.Operation)) {
	case "list":
		return t.listMessages(in)
	case "get", "read":
		return t.getMessage(in)
	case "wait":
		return t.waitForMessage(in)
	case "delete":
		return t.deleteMessage(in)
	case "send":
		return t.sendMessage(ctx, in)
	default:
		return errorJSON("unsupported operation: " + strings.TrimSpace(in.Operation))
	}
}

func (t *EmailTool) IsImmediate() bool {
	return false
}

func (t *EmailTool) GetParameters() Parameters {
	return Parameters{
		Type: "object",
		Properties: map[string]any{
			"operation": map[string]any{
				"type":        "string",
				"description": "Operation: list, get/read, wait, delete, send.",
				"enum":        []string{"list", "get", "read", "wait", "delete", "send"},
			},
			"id": map[string]any{
				"type":        "integer",
				"description": "POP3 message number for get/delete.",
				"minimum":     1,
			},
			"uidl": map[string]any{
				"type":        "string",
				"description": "POP3 UIDL for get/delete.",
			},
			"after_id": map[string]any{
				"type":        "integer",
				"description": "Only match messages with id greater than this value.",
				"minimum":     0,
			},
			"from_contains": map[string]any{
				"type":        "string",
				"description": "Case-insensitive sender filter for list/wait.",
			},
			"subject_contains": map[string]any{
				"type":        "string",
				"description": "Case-insensitive subject filter for list/wait.",
			},
			"body_contains": map[string]any{
				"type":        "string",
				"description": "Case-insensitive text/html body filter for list/wait.",
			},
			"link_contains": map[string]any{
				"type":        "string",
				"description": "Case-insensitive substring filter over extracted links for list/wait.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "For list, number of newest matching messages to return (1-20).",
				"minimum":     1,
				"maximum":     maxEmailListLimit,
			},
			"timeout_seconds": map[string]any{
				"type":        "integer",
				"description": "For wait, total polling timeout in seconds.",
				"minimum":     1,
				"maximum":     1800,
			},
			"poll_seconds": map[string]any{
				"type":        "integer",
				"description": "For wait, poll interval in seconds.",
				"minimum":     1,
				"maximum":     300,
			},
			"max_body_chars": map[string]any{
				"type":        "integer",
				"description": "Maximum characters returned for text/html bodies.",
				"minimum":     100,
				"maximum":     maxEmailBodyChars,
			},
			"include_body": map[string]any{
				"type":        "boolean",
				"description": "For get/wait, include text and html bodies in the response.",
			},
			"to": map[string]any{
				"type":        "array",
				"description": "For send, recipient addresses.",
				"items":       map[string]any{"type": "string"},
			},
			"cc": map[string]any{
				"type":        "array",
				"description": "For send, cc recipient addresses.",
				"items":       map[string]any{"type": "string"},
			},
			"bcc": map[string]any{
				"type":        "array",
				"description": "For send, bcc recipient addresses.",
				"items":       map[string]any{"type": "string"},
			},
			"subject": map[string]any{
				"type":        "string",
				"description": "For send, email subject.",
			},
			"text": map[string]any{
				"type":        "string",
				"description": "For send, plain-text message body.",
			},
			"html": map[string]any{
				"type":        "string",
				"description": "For send, HTML message body.",
			},
			"reply_to": map[string]any{
				"type":        "string",
				"description": "For send, optional Reply-To header.",
			},
		},
		Required: []string{"operation"},
	}
}

func (t *EmailTool) listMessages(in emailParams) string {
	if err := t.requirePOP3(); err != nil {
		return errorJSON(err.Error())
	}
	limit := clampEmailLimit(in.Limit)

	client, err := t.openPOP3()
	if err != nil {
		return errorJSON(err.Error())
	}
	defer client.close()

	count, err := client.stat()
	if err != nil {
		return errorJSON(err.Error())
	}

	start := max(1, count-limit*4)
	msgs := make([]emailMessage, 0, limit)
	for id := count; id >= start && len(msgs) < limit; id-- {
		msg, msgErr := client.retrieveParsed(id, false, defaultEmailBodyChars)
		if msgErr != nil {
			return errorJSON(msgErr.Error())
		}
		if !matchesEmailFilters(msg, in) {
			continue
		}
		msg.TextBody = ""
		msg.HTMLBody = ""
		msgs = append(msgs, msg)
	}

	slices.Reverse(msgs)
	return mustJSON(map[string]any{
		"operation": "list",
		"count":     len(msgs),
		"messages":  msgs,
	})
}

func (t *EmailTool) getMessage(in emailParams) string {
	if err := t.requirePOP3(); err != nil {
		return errorJSON(err.Error())
	}
	includeBody := in.IncludeBody
	maxBodyChars := clampBodyChars(in.MaxBodyChars)

	client, err := t.openPOP3()
	if err != nil {
		return errorJSON(err.Error())
	}
	defer client.close()

	id, err := resolvePOP3ID(client, in.ID, in.UIDL)
	if err != nil {
		return errorJSON(err.Error())
	}
	msg, err := client.retrieveParsed(id, includeBody, maxBodyChars)
	if err != nil {
		return errorJSON(err.Error())
	}
	return mustJSON(map[string]any{
		"operation": "get",
		"message":   msg,
	})
}

func (t *EmailTool) waitForMessage(in emailParams) string {
	if err := t.requirePOP3(); err != nil {
		return errorJSON(err.Error())
	}
	timeoutSeconds := in.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultEmailWaitTimeout
	}
	pollSeconds := in.PollSeconds
	if pollSeconds <= 0 {
		pollSeconds = defaultEmailPollSeconds
	}
	maxBodyChars := clampBodyChars(in.MaxBodyChars)

	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for {
		client, err := t.openPOP3()
		if err != nil {
			return errorJSON(err.Error())
		}

		count, err := client.stat()
		if err != nil {
			client.close()
			return errorJSON(err.Error())
		}

		for id := count; id >= 1; id-- {
			if in.AfterID > 0 && id <= in.AfterID {
				break
			}
			msg, msgErr := client.retrieveParsed(id, true, maxBodyChars)
			if msgErr != nil {
				client.close()
				return errorJSON(msgErr.Error())
			}
			if !matchesEmailFilters(msg, in) {
				continue
			}
			if !in.IncludeBody {
				msg.TextBody = ""
				msg.HTMLBody = ""
			}
			client.close()
			return mustJSON(map[string]any{
				"operation": "wait",
				"status":    "matched",
				"message":   msg,
			})
		}
		client.close()

		if time.Now().After(deadline) {
			return mustJSON(map[string]any{
				"operation": "wait",
				"status":    "timeout",
			})
		}
		time.Sleep(time.Duration(pollSeconds) * time.Second)
	}
}

func (t *EmailTool) deleteMessage(in emailParams) string {
	if err := t.requirePOP3(); err != nil {
		return errorJSON(err.Error())
	}
	client, err := t.openPOP3()
	if err != nil {
		return errorJSON(err.Error())
	}
	defer client.close()

	id, err := resolvePOP3ID(client, in.ID, in.UIDL)
	if err != nil {
		return errorJSON(err.Error())
	}
	if err := client.dele(id); err != nil {
		return errorJSON(err.Error())
	}
	if err := client.quit(); err != nil {
		return errorJSON(err.Error())
	}
	return mustJSON(map[string]any{
		"operation": "delete",
		"status":    "deleted",
		"id":        id,
	})
}

func (t *EmailTool) sendMessage(parentCtx context.Context, in emailParams) string {
	if err := t.requireSMTP(); err != nil {
		return errorJSON(err.Error())
	}
	to := cleanEmailAddrs(in.To)
	cc := cleanEmailAddrs(in.Cc)
	bcc := cleanEmailAddrs(in.Bcc)
	if len(to)+len(cc)+len(bcc) == 0 && t.persona != nil {
		ctx, cancel := context.WithTimeout(parentCtx, 3*time.Second)
		defer cancel()
		if fact, ok, err := t.persona.BestFact(ctx, "email"); err == nil && ok && strings.TrimSpace(fact.Value) != "" {
			to = []string{strings.TrimSpace(fact.Value)}
		}
	}
	if len(to)+len(cc)+len(bcc) == 0 {
		return errorJSON("at least one recipient is required")
	}
	textBody := strings.TrimSpace(in.Text)
	htmlBody := strings.TrimSpace(in.HTML)
	if textBody == "" && htmlBody == "" {
		return errorJSON("text or html is required")
	}
	if htmlBody != "" {
		textBody = firstNonEmptyTrimmed(textBody, htmlToText(htmlBody))
	}
	textBody = normalizeOutgoingEmailText(textBody)
	if textBody == "" {
		return errorJSON("email body is empty after plain-text normalization")
	}

	msg, from, recipients, err := t.buildSMTPMessage(in.Subject, textBody, in.ReplyTo, to, cc, bcc)
	if err != nil {
		return errorJSON(err.Error())
	}
	if err := t.smtpSend(from, recipients, msg); err != nil {
		return errorJSON(err.Error())
	}
	return mustJSON(map[string]any{
		"operation":  "send",
		"status":     "sent",
		"from":       from,
		"recipients": recipients,
		"subject":    strings.TrimSpace(in.Subject),
	})
}

func (t *EmailTool) requirePOP3() error {
	if t.pop3.Host == "" || t.pop3.Username == "" || t.pop3.Password == "" {
		return fmt.Errorf("pop3 is not configured")
	}
	return nil
}

func (t *EmailTool) requireSMTP() error {
	if t.smtp.Host == "" || t.smtp.From == "" {
		return fmt.Errorf("smtp is not configured")
	}
	return nil
}

func (t *EmailTool) openPOP3() (*pop3Client, error) {
	client, err := dialPOP3(t.pop3)
	if err != nil {
		return nil, err
	}
	if err := client.login(); err != nil {
		client.close()
		return nil, err
	}
	return client, nil
}

func (t *EmailTool) buildSMTPMessage(subject string, textBody string, replyTo string, to []string, cc []string, bcc []string) ([]byte, string, []string, error) {
	from := strings.TrimSpace(t.smtp.From)
	if from == "" {
		return nil, "", nil, fmt.Errorf("smtp from address is not configured")
	}
	recipients := append(append(append([]string{}, to...), cc...), bcc...)
	if len(recipients) == 0 {
		return nil, "", nil, fmt.Errorf("at least one recipient is required")
	}

	var body bytes.Buffer
	body.WriteString(fmt.Sprintf("From: %s\r\n", from))
	if len(to) > 0 {
		body.WriteString(fmt.Sprintf("To: %s\r\n", strings.Join(to, ", ")))
	}
	if len(cc) > 0 {
		body.WriteString(fmt.Sprintf("Cc: %s\r\n", strings.Join(cc, ", ")))
	}
	body.WriteString(fmt.Sprintf("Subject: %s\r\n", encodeMailHeader(subject)))
	body.WriteString(fmt.Sprintf("Date: %s\r\n", time.Now().Format(time.RFC1123Z)))
	body.WriteString("MIME-Version: 1.0\r\n")
	if rt := strings.TrimSpace(replyTo); rt != "" {
		body.WriteString(fmt.Sprintf("Reply-To: %s\r\n", rt))
	}
	body.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	body.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	body.WriteString(textBody)
	body.WriteString("\r\n")
	return body.Bytes(), from, recipients, nil
}

func (t *EmailTool) smtpSend(from string, recipients []string, msg []byte) error {
	addr := net.JoinHostPort(t.smtp.Host, strconv.Itoa(t.smtp.Port))
	dialer := &net.Dialer{Timeout: t.smtp.Timeout}

	var conn net.Conn
	var err error
	if t.smtp.UseImplicitTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, secureTLSConfig(t.smtp.Host))
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial failed: %w", err)
	}
	defer conn.Close()

	client, err := smtp.NewClient(conn, t.smtp.Host)
	if err != nil {
		return fmt.Errorf("smtp client init failed: %w", err)
	}
	defer client.Quit()

	if !t.smtp.UseImplicitTLS && t.smtp.UseSTARTTLS {
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf("smtp server does not support STARTTLS")
		}
		if err := client.StartTLS(secureTLSConfig(t.smtp.Host)); err != nil {
			return fmt.Errorf("smtp starttls failed: %w", err)
		}
	}

	if auth := t.smtpAuth(); auth != nil {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("smtp server does not support AUTH")
		}
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth failed: %w", err)
		}
	}

	if err := client.Mail(from); err != nil {
		return fmt.Errorf("smtp MAIL FROM failed: %w", err)
	}
	for _, rcpt := range recipients {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp RCPT TO failed for %s: %w", rcpt, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA failed: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return fmt.Errorf("smtp message write failed: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp finalize failed: %w", err)
	}
	return nil
}

func (t *EmailTool) smtpAuth() smtp.Auth {
	if t.smtp.Username == "" || t.smtp.Password == "" || t.smtp.AuthMethod == "none" {
		return nil
	}
	switch t.smtp.AuthMethod {
	case "login":
		return &loginAuth{username: t.smtp.Username, password: t.smtp.Password}
	default:
		return smtp.PlainAuth("", t.smtp.Username, t.smtp.Password, t.smtp.Host)
	}
}

type pop3Client struct {
	cfg    emailPOP3Config
	conn   net.Conn
	reader *textproto.Reader
	writer *textproto.Writer
}

func dialPOP3(cfg emailPOP3Config) (*pop3Client, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dialer := &net.Dialer{Timeout: cfg.Timeout}

	var conn net.Conn
	var err error
	if cfg.UseTLS {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, secureTLSConfig(cfg.Host))
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("pop3 dial failed: %w", err)
	}

	client := &pop3Client{
		cfg:    cfg,
		conn:   conn,
		reader: textproto.NewReader(bufio.NewReader(conn)),
		writer: textproto.NewWriter(bufio.NewWriter(conn)),
	}
	line, err := client.reader.ReadLine()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("pop3 greeting failed: %w", err)
	}
	if !strings.HasPrefix(line, "+OK") {
		_ = conn.Close()
		return nil, fmt.Errorf("pop3 greeting error: %s", line)
	}
	return client, nil
}

func secureTLSConfig(serverName string) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: serverName,
	}
}

func (c *pop3Client) login() error {
	if _, err := c.cmd("USER " + c.cfg.Username); err != nil {
		return fmt.Errorf("pop3 USER failed: %w", err)
	}
	if _, err := c.cmd("PASS " + c.cfg.Password); err != nil {
		return fmt.Errorf("pop3 PASS failed: %w", err)
	}
	return nil
}

func (c *pop3Client) stat() (int, error) {
	line, err := c.cmd("STAT")
	if err != nil {
		return 0, fmt.Errorf("pop3 STAT failed: %w", err)
	}
	parts := strings.Fields(line)
	if len(parts) < 2 {
		return 0, fmt.Errorf("unexpected STAT response: %s", line)
	}
	count, convErr := strconv.Atoi(parts[1])
	if convErr != nil {
		return 0, fmt.Errorf("invalid STAT count: %w", convErr)
	}
	return count, nil
}

func (c *pop3Client) uidl(id int) (string, error) {
	line, err := c.cmd(fmt.Sprintf("UIDL %d", id))
	if err != nil {
		return "", fmt.Errorf("pop3 UIDL failed: %w", err)
	}
	parts := strings.Fields(line)
	if len(parts) < 3 {
		return "", fmt.Errorf("unexpected UIDL response: %s", line)
	}
	return strings.TrimSpace(parts[2]), nil
}

func (c *pop3Client) findByUIDL(uidl string) (int, error) {
	lines, err := c.cmdMulti("UIDL")
	if err != nil {
		return 0, fmt.Errorf("pop3 UIDL listing failed: %w", err)
	}
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if strings.TrimSpace(parts[1]) != strings.TrimSpace(uidl) {
			continue
		}
		id, convErr := strconv.Atoi(parts[0])
		if convErr != nil {
			return 0, fmt.Errorf("invalid UIDL response: %w", convErr)
		}
		return id, nil
	}
	return 0, fmt.Errorf("uidl not found: %s", uidl)
}

func (c *pop3Client) retr(id int) ([]byte, error) {
	lines, err := c.cmdMulti(fmt.Sprintf("RETR %d", id))
	if err != nil {
		return nil, fmt.Errorf("pop3 RETR failed: %w", err)
	}
	var buf bytes.Buffer
	for i, line := range lines {
		if i > 0 {
			buf.WriteString("\r\n")
		}
		buf.WriteString(line)
	}
	return buf.Bytes(), nil
}

func (c *pop3Client) dele(id int) error {
	if _, err := c.cmd(fmt.Sprintf("DELE %d", id)); err != nil {
		return fmt.Errorf("pop3 DELE failed: %w", err)
	}
	return nil
}

func (c *pop3Client) quit() error {
	if _, err := c.cmd("QUIT"); err != nil {
		return fmt.Errorf("pop3 QUIT failed: %w", err)
	}
	return nil
}

func (c *pop3Client) close() {
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

func (c *pop3Client) retrieveParsed(id int, includeBody bool, maxBodyChars int) (emailMessage, error) {
	raw, err := c.retr(id)
	if err != nil {
		return emailMessage{}, err
	}
	uidl, _ := c.uidl(id)
	msg, err := parseEmailMessage(id, uidl, raw, includeBody, maxBodyChars)
	if err != nil {
		return emailMessage{}, err
	}
	return msg, nil
}

func (c *pop3Client) cmd(command string) (string, error) {
	if err := c.writer.PrintfLine("%s", command); err != nil {
		return "", err
	}
	line, err := c.reader.ReadLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "+OK") {
		return "", fmt.Errorf("%s", strings.TrimSpace(line))
	}
	return line, nil
}

func (c *pop3Client) cmdMulti(command string) ([]string, error) {
	if _, err := c.cmd(command); err != nil {
		return nil, err
	}
	lines := make([]string, 0, 32)
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		if line == "." {
			return lines, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		lines = append(lines, line)
	}
}

func resolvePOP3ID(client *pop3Client, id int, uidl string) (int, error) {
	if id > 0 {
		return id, nil
	}
	if strings.TrimSpace(uidl) == "" {
		return 0, fmt.Errorf("id or uidl is required")
	}
	return client.findByUIDL(uidl)
}

func parseEmailMessage(id int, uidl string, raw []byte, includeBody bool, maxBodyChars int) (emailMessage, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return emailMessage{}, fmt.Errorf("parse email failed: %w", err)
	}
	header := parsed.Header

	subject := decodeMailHeader(header.Get("Subject"))
	from := decodeMailHeader(header.Get("From"))
	dateRaw := header.Get("Date")
	messageID := strings.TrimSpace(header.Get("Message-Id"))
	to := normalizeAddressHeader(header.Get("To"))

	msg := emailMessage{
		ID:        id,
		UIDL:      uidl,
		Subject:   subject,
		From:      from,
		To:        to,
		Date:      strings.TrimSpace(dateRaw),
		MessageID: messageID,
	}

	mediaType, params, _ := mime.ParseMediaType(header.Get("Content-Type"))
	textBody, htmlBody, err := extractBodies(parsed.Body, mediaType, params)
	if err != nil {
		return emailMessage{}, fmt.Errorf("extract email body failed: %w", err)
	}

	allLinks := uniqueStrings(append(extractLinks(textBody), extractLinks(htmlBody)...))
	preview := firstNonEmptyTrimmed(textBody, htmlToText(htmlBody))
	msg.Preview = truncateText(preview, 280)
	msg.Links = allLinks
	if includeBody {
		msg.TextBody = truncateText(textBody, maxBodyChars)
		msg.HTMLBody = truncateText(htmlBody, maxBodyChars)
	}
	return msg, nil
}

func extractBodies(body io.Reader, mediaType string, params map[string]string) (string, string, error) {
	switch {
	case strings.HasPrefix(mediaType, "multipart/"):
		boundary := params["boundary"]
		if boundary == "" {
			return "", "", nil
		}
		mr := multipart.NewReader(body, boundary)
		var textBody string
		var htmlBody string
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", "", err
			}
			pt, pp, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			partBody, err := decodeMIMEBody(part.Header, part)
			if err != nil {
				return "", "", err
			}
			if strings.HasPrefix(pt, "multipart/") {
				nestedText, nestedHTML, err := extractBodies(bytes.NewReader(partBody), pt, pp)
				if err != nil {
					return "", "", err
				}
				if textBody == "" {
					textBody = nestedText
				}
				if htmlBody == "" {
					htmlBody = nestedHTML
				}
				continue
			}
			switch strings.ToLower(strings.TrimSpace(pt)) {
			case "text/plain":
				if textBody == "" {
					textBody = strings.TrimSpace(string(partBody))
				}
			case "text/html":
				if htmlBody == "" {
					htmlBody = strings.TrimSpace(string(partBody))
				}
			}
		}
		return textBody, htmlBody, nil
	case mediaType == "text/html":
		data, err := decodeMIMEBody(textproto.MIMEHeader{}, body)
		return "", strings.TrimSpace(string(data)), err
	default:
		data, err := decodeMIMEBody(textproto.MIMEHeader{}, body)
		return strings.TrimSpace(string(data)), "", err
	}
}

func decodeMIMEBody(header textproto.MIMEHeader, body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	encoding := strings.ToLower(strings.TrimSpace(header.Get("Content-Transfer-Encoding")))
	switch encoding {
	case "base64":
		dst := make([]byte, base64.StdEncoding.DecodedLen(len(raw)))
		n, err := base64.StdEncoding.Decode(dst, bytes.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		return dst[:n], nil
	case "quoted-printable":
		r := quotedPrintableReader(bytes.NewReader(raw))
		return io.ReadAll(r)
	default:
		return raw, nil
	}
}

func matchesEmailFilters(msg emailMessage, in emailParams) bool {
	if in.AfterID > 0 && msg.ID <= in.AfterID {
		return false
	}
	if !containsFold(msg.From, in.FromContains) {
		return false
	}
	if !containsFold(msg.Subject, in.SubjectContains) {
		return false
	}
	if !containsFold(msg.TextBody+" "+htmlToText(msg.HTMLBody), in.BodyContains) {
		return false
	}
	if strings.TrimSpace(in.LinkContains) != "" {
		ok := false
		for _, link := range msg.Links {
			if containsFold(link, in.LinkContains) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func extractLinks(content string) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	out := make([]string, 0, 8)
	for _, match := range linkHrefPattern.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			out = append(out, html.UnescapeString(strings.TrimSpace(match[1])))
		}
	}
	for _, match := range linkTextPattern.FindAllString(content, -1) {
		out = append(out, strings.TrimSpace(match))
	}
	return uniqueStrings(out)
}

func htmlToText(content string) string {
	if strings.TrimSpace(content) == "" {
		return ""
	}
	out := html.UnescapeString(content)
	out = htmlTagPattern.ReplaceAllString(out, " ")
	out = spacePattern.ReplaceAllString(out, " ")
	return strings.TrimSpace(out)
}

func normalizeOutgoingEmailText(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = stripMarkdownFormatting(line)
		line = strings.TrimSpace(spacePattern.ReplaceAllString(line, " "))
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func stripMarkdownFormatting(line string) string {
	replacer := strings.NewReplacer(
		"**", "",
		"__", "",
		"`", "",
		"~~", "",
		"# ", "",
		"## ", "",
		"### ", "",
		"#### ", "",
		"##### ", "",
		"###### ", "",
		"> ", "",
		"* ", "- ",
		"- [ ] ", "- ",
		"- [x] ", "- ",
	)
	line = replacer.Replace(line)
	line = regexp.MustCompile(`\[(.*?)\]\((.*?)\)`).ReplaceAllString(line, "$1: $2")
	line = regexp.MustCompile(`!\[(.*?)\]\((.*?)\)`).ReplaceAllString(line, "$1")
	line = regexp.MustCompile(`(^|\s)[*_]+([^*_]+)[*_]+`).ReplaceAllString(line, "$1$2")
	return line
}

func normalizeAddressHeader(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	addrs, err := mail.ParseAddressList(raw)
	if err != nil {
		return []string{decodeMailHeader(raw)}
	}
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.String())
	}
	return out
}

func cleanEmailAddrs(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func decodeMailHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	decoded, err := new(mime.WordDecoder).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

func encodeMailHeader(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return mime.BEncoding.Encode("UTF-8", value)
}

func firstNonEmptyTrimmed(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func containsFold(haystack string, needle string) bool {
	needle = strings.TrimSpace(needle)
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func truncateText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if limit <= 0 || len(value) <= limit {
		return value
	}
	return strings.TrimSpace(value[:limit]) + "..."
}

func clampEmailLimit(limit int) int {
	if limit <= 0 {
		return defaultEmailListLimit
	}
	if limit > maxEmailListLimit {
		return maxEmailListLimit
	}
	return limit
}

func clampBodyChars(n int) int {
	if n <= 0 {
		return defaultEmailBodyChars
	}
	if n > maxEmailBodyChars {
		return maxEmailBodyChars
	}
	return n
}

func normalizeEmailAuthMethod(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "plain", "auto":
		return "plain"
	case "login":
		return "login"
	case "none":
		return "none"
	default:
		return "plain"
	}
}

func envBoolDefault(key string, fallback bool) bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch raw {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return fallback
	}
}

func envIntDefault(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

type loginAuth struct {
	username string
	password string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", []byte(a.username), nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	prompt := strings.ToLower(string(fromServer))
	if strings.Contains(prompt, "username") {
		return []byte(a.username), nil
	}
	if strings.Contains(prompt, "password") {
		return []byte(a.password), nil
	}
	if prompt == "" {
		return []byte(a.password), nil
	}
	return nil, fmt.Errorf("unexpected smtp login challenge: %q", string(fromServer))
}

func quotedPrintableReader(r io.Reader) io.Reader {
	return quotedprintable.NewReader(r)
}
