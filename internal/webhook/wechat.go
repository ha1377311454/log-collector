package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

// Client 通过企业微信群机器人 Webhook 推送配置级别的日志。
type Client struct {
	url               string
	title             string
	environment       string
	hostname          string
	maxContentLength  int
	http              *http.Client
	now               func() time.Time
	ignoreKeywords    []string
	errorTypeKeywords []string
	severityLevels    map[string]struct{}
	log               *logging.Logger
}

const (
	defaultTitle   = "日志异常告警"
	nodeIPEnv      = "NODE_IP"
	warningOpen    = `<font color="warning">`
	warningClose   = `</font>`
	hostNameKey    = "host.name"
	sourceRuleKey  = "log.source.rule"
	logFilePathKey = "log.file.path"
	requestIDKey   = "request.id"
)

var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	"`", "\\`",
	"<", "\\<",
	">", "\\>",
)

type markdownMessage struct {
	MsgType  string          `json:"msgtype"`
	Markdown markdownContent `json:"markdown"`
}

type markdownContent struct {
	Content string `json:"content"`
}

type webhookResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

func New(cfg config.WebhookConfig, logger *logging.Logger) (*Client, error) {
	parsed, err := url.Parse(cfg.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("invalid WeChat webhook URL")
	}
	hostname := strings.TrimSpace(os.Getenv(nodeIPEnv))
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	if logger == nil {
		logger = logging.Nop()
	}
	severityLevels := cfg.SeverityLevels
	if len(severityLevels) == 0 {
		severityLevels = []string{"ERROR", "FATAL"}
	}
	levelSet := make(map[string]struct{}, len(severityLevels))
	for _, level := range severityLevels {
		levelSet[strings.ToUpper(level)] = struct{}{}
	}
	return &Client{
		url:               cfg.URL,
		title:             cfg.Title,
		environment:       strings.TrimSpace(cfg.Environment),
		hostname:          hostname,
		maxContentLength:  cfg.MaxContentLength,
		http:              &http.Client{Timeout: cfg.Timeout.Duration},
		now:               time.Now,
		ignoreKeywords:    append([]string(nil), cfg.IgnoreKeywords...),
		errorTypeKeywords: append([]string(nil), cfg.ErrorTypeKeywords...),
		severityLevels:    levelSet,
		log:               logger,
	}, nil
}

// Send 忽略未配置级别；匹配的日志使用企业微信群机器人 markdown 消息格式同步推送。
func (c *Client) Send(ctx context.Context, record model.Record) error {
	if _, enabled := c.severityLevels[record.SeverityText]; !enabled {
		return nil
	}
	if keyword, ignored := c.ignoredKeyword(record.Body); ignored {
		c.log.Debug("WeChat webhook ignored log by keyword",
			"keyword", keyword,
			"severity", record.SeverityText,
			"source", record.ResourceAttributes[sourceRuleKey],
			"file", record.ResourceAttributes[logFilePathKey],
			"request_id", record.Attributes[requestIDKey],
			"body", record.Body,
		)
		return nil
	}
	payload, err := json.Marshal(markdownMessage{MsgType: "markdown", Markdown: markdownContent{Content: c.content(record)}})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return errors.New("create WeChat webhook request failed")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error 会包含带机器人 key 的完整 URL，不能继续包装到运行日志。
		return errors.New("send WeChat webhook request failed")
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if readErr != nil {
		return fmt.Errorf("read WeChat webhook response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("WeChat webhook HTTP status %d", resp.StatusCode)
	}
	var result webhookResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("decode WeChat webhook response: %w", err)
	}
	if result.ErrCode != 0 {
		return fmt.Errorf("WeChat webhook rejected message: errcode=%d errmsg=%s", result.ErrCode, result.ErrMsg)
	}
	return nil
}

func (c *Client) ignoredKeyword(body string) (string, bool) {
	keyword := firstMatchingKeyword(body, c.ignoreKeywords)
	return keyword, keyword != ""
}

func firstMatchingKeyword(body string, keywords []string) string {
	for _, keyword := range keywords {
		if strings.Contains(body, keyword) {
			return keyword
		}
	}
	return ""
}

func (c *Client) content(record model.Record) string {
	timestamp := record.Timestamp
	if timestamp.IsZero() {
		timestamp = c.now()
	}
	alert := markdownAlert{
		title: firstNonEmpty(c.title, defaultTitle),
		fields: []alertField{
			{label: "环境", value: c.environment},
			{label: "主机", value: firstNonEmpty(record.ResourceAttributes[hostNameKey], c.hostname)},
			{label: "时间", value: timestamp.Format("2006-01-02 15:04:05")},
			{label: "日志级别", value: record.SeverityText, highlighted: true},
			{label: "错误类型", value: firstMatchingKeyword(record.Body, c.errorTypeKeywords), highlighted: true},
			{label: "日志来源", value: record.ResourceAttributes[sourceRuleKey]},
			{label: "请求 ID", value: record.Attributes[requestIDKey], highlighted: true},
			{label: "日志文件", value: record.ResourceAttributes[logFilePathKey]},
		},
		body: record.Body,
	}
	return alert.render(c.maxContentLength)
}

type markdownAlert struct {
	title  string
	fields []alertField
	body   string
}

type alertField struct {
	label       string
	value       string
	highlighted bool
}

func (a markdownAlert) render(limit int) string {
	writer := newMarkdownWriter(limit)
	writer.line("### 🚨 ", escape(a.title), "")
	for _, field := range a.fields {
		writer.field(field)
	}
	writer.blankLine()
	writer.line("**日志内容**", "", "")
	for _, line := range strings.Split(a.body, "\n") {
		if !writer.line("> "+warningOpen, escape(line), warningClose) {
			break
		}
	}
	return writer.String()
}

type markdownWriter struct {
	content   strings.Builder
	remaining int
}

func newMarkdownWriter(limit int) *markdownWriter {
	return &markdownWriter{remaining: limit}
}

func (w *markdownWriter) field(field alertField) {
	if field.value == "" {
		return
	}
	prefix, suffix := "> **"+field.label+"：** ", ""
	if field.highlighted {
		prefix += warningOpen
		suffix = warningClose
	}
	w.line(prefix, escape(field.value), suffix)
}

func (w *markdownWriter) line(prefix, value, suffix string) bool {
	separator := ""
	if w.content.Len() > 0 {
		separator = "\n"
	}
	fixedLength := runeLen(separator) + runeLen(prefix) + runeLen(suffix)
	if w.remaining < fixedLength {
		return false
	}
	rendered := truncate(value, w.remaining-fixedLength)
	w.content.WriteString(separator)
	w.content.WriteString(prefix)
	w.content.WriteString(rendered)
	w.content.WriteString(suffix)
	w.remaining -= fixedLength + runeLen(rendered)
	return true
}

func (w *markdownWriter) blankLine() {
	if w.remaining > 0 && w.content.Len() > 0 {
		w.content.WriteByte('\n')
		w.remaining--
	}
}

func (w *markdownWriter) String() string { return w.content.String() }

// escape 只转义会改变日志展示语义的 Markdown/HTML 定界符；转义符本身不会显示。
func escape(value string) string {
	return markdownEscaper.Replace(value)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func runeLen(value string) int { return len([]rune(value)) }

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	const suffix = "...[truncated]"
	suffixRunes := []rune(suffix)
	if limit <= len(suffixRunes) {
		return string(runes[:limit])
	}
	return string(runes[:limit-len(suffixRunes)]) + suffix
}
