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
	"log-collector/internal/model"
)

// Client 通过企业微信群机器人 Webhook 推送 ERROR 和 FATAL 日志。
type Client struct {
	url              string
	title            string
	hostname         string
	maxContentLength int
	http             *http.Client
}

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

func New(cfg config.WebhookConfig) (*Client, error) {
	parsed, err := url.Parse(cfg.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("invalid WeChat webhook URL")
	}
	hostname, _ := os.Hostname()
	return &Client{url: cfg.URL, title: cfg.Title, hostname: hostname, maxContentLength: cfg.MaxContentLength, http: &http.Client{Timeout: cfg.Timeout.Duration}}, nil
}

// Send 忽略非错误级别；错误日志使用企业微信群机器人 markdown 消息格式同步推送。
func (c *Client) Send(ctx context.Context, record model.Record) error {
	if record.SeverityText != "ERROR" && record.SeverityText != "FATAL" {
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

func (c *Client) content(record model.Record) string {
	title := c.title
	if title == "" {
		title = "日志异常告警"
	}
	hostname := record.ResourceAttributes["host.name"]
	if hostname == "" {
		hostname = c.hostname
	}
	timestamp := record.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	var header strings.Builder
	fmt.Fprintf(&header, "### 🚨 %s", escape(title))
	if hostname != "" {
		fmt.Fprintf(&header, "\n> **主机：** %s", escape(hostname))
	}
	fmt.Fprintf(&header, "\n> **时间：** %s", timestamp.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&header, "\n> **日志级别：** <font color=\"warning\">%s</font>", escape(record.SeverityText))
	if value := record.ResourceAttributes["log.source.rule"]; value != "" {
		fmt.Fprintf(&header, "\n> **日志来源：** %s", escape(value))
	}
	if value := record.Attributes["request.id"]; value != "" {
		fmt.Fprintf(&header, "\n> **请求 ID：** <font color=\"warning\">%s</font>", escape(value))
	}
	if value := record.ResourceAttributes["log.file.path"]; value != "" {
		fmt.Fprintf(&header, "\n> **日志文件：** %s", escape(value))
	}
	header.WriteString("\n\n**日志内容**")
	return appendBodyLines(header.String(), record.Body, c.maxContentLength)
}

// escape 只转义会改变日志展示语义的 Markdown/HTML 定界符；转义符本身不会显示。
func escape(value string) string {
	return strings.NewReplacer(
		`\`, `\\`,
		"`", "\\`",
		"<", "\\<",
		">", "\\>",
	).Replace(value)
}

func appendBodyLines(header, body string, limit int) string {
	var content strings.Builder
	content.WriteString(header)
	used := len([]rune(header))
	for _, line := range strings.Split(body, "\n") {
		const linePrefix = "\n> <font color=\"warning\">"
		const lineSuffix = "</font>"
		wrapperLength := len([]rune(linePrefix)) + len([]rune(lineSuffix))
		remaining := limit - used - wrapperLength
		if remaining <= 0 {
			break
		}
		rendered := truncate(escape(line), remaining)
		content.WriteString(linePrefix)
		content.WriteString(rendered)
		content.WriteString(lineSuffix)
		used += wrapperLength + len([]rune(rendered))
		if used >= limit {
			break
		}
	}
	return content.String()
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	const suffix = "\n...[truncated]"
	suffixRunes := []rune(suffix)
	if limit <= len(suffixRunes) {
		return string(runes[:limit])
	}
	return string(runes[:limit-len(suffixRunes)]) + suffix
}
