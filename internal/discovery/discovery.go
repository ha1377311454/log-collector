package discovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

type compiledProcessRule struct {
	rule                            config.ProcessRule
	comm, cmdline, include, exclude *regexp.Regexp
	extractors                      []model.AttributeExtractor
	dropLevels                      map[string]struct{}
}

type compiledFileRule struct {
	rule       config.FileRule
	extractors []model.AttributeExtractor
	dropLevels map[string]struct{}
}

// Discovery 使用 gopsutil 获取进程快照，并结合文件 glob 发现日志源。
// 所有正则只在初始化时编译一次，发现热路径不再重复编译。
type Discovery struct {
	processRules []compiledProcessRule
	files        []compiledFileRule
	containers   []compiledFileRule
	cacheTTL     time.Duration
	log          *logging.Logger
	mu           sync.Mutex
	seen         map[string]time.Time
}

func New(cfg config.SourcesConfig, cacheTTL time.Duration, logger *logging.Logger) (*Discovery, error) {
	d := &Discovery{cacheTTL: cacheTTL, log: logger, seen: make(map[string]time.Time)}
	for _, rule := range cfg.Processes {
		compiled := compiledProcessRule{rule: rule}
		var err error
		if compiled.comm, err = optionalRegexp(rule.CommRegex); err != nil {
			return nil, err
		}
		if compiled.cmdline, err = optionalRegexp(rule.CmdlineRegex); err != nil {
			return nil, err
		}
		if compiled.include, err = regexp.Compile(rule.IncludeRegex); err != nil {
			return nil, err
		}
		if compiled.exclude, err = optionalRegexp(rule.ExcludeRegex); err != nil {
			return nil, err
		}
		if compiled.extractors, err = compileExtractors(rule.Extractors); err != nil {
			return nil, err
		}
		compiled.dropLevels = dropLevels(rule.DropLevels)
		d.processRules = append(d.processRules, compiled)
	}
	for _, rule := range cfg.Files {
		extractors, err := compileExtractors(rule.Extractors)
		if err != nil {
			return nil, err
		}
		d.files = append(d.files, compiledFileRule{rule: rule, extractors: extractors, dropLevels: dropLevels(rule.DropLevels)})
	}
	for _, rule := range cfg.Containers {
		extractors, err := compileExtractors(rule.Extractors)
		if err != nil {
			return nil, err
		}
		d.containers = append(d.containers, compiledFileRule{rule: rule, extractors: extractors, dropLevels: dropLevels(rule.DropLevels)})
	}
	return d, nil
}

// Discover 每轮只获取一次进程列表。每个匹配进程也只调用一次 OpenFiles，
// 然后将同一份打开文件快照交给所有命中的进程规则过滤。
func (d *Discovery) Discover(ctx context.Context) ([]model.FileTarget, error) {
	var targets []model.FileTarget
	var errs []error
	// 没有进程规则时完全跳过进程枚举，纯文件采集不承担额外系统调用开销。
	if len(d.processRules) > 0 {
		processes, err := process.ProcessesWithContext(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("list processes: %w", err))
		}
		for _, p := range processes {
			found, findErr := d.discoverProcess(ctx, p)
			if findErr != nil {
				errs = append(errs, findErr)
			}
			targets = append(targets, found...)
		}
	}
	for _, rule := range d.files {
		targets = append(targets, discoverFiles(rule, "file")...)
	}
	for _, rule := range d.containers {
		targets = append(targets, discoverFiles(rule, "container")...)
	}
	return d.deduplicate(targets), errorsJoin(errs)
}

func (d *Discovery) discoverProcess(ctx context.Context, p *process.Process) ([]model.FileTarget, error) {
	comm, err := p.NameWithContext(ctx)
	if err != nil {
		return nil, nil
	}
	// 先用进程名排除不可能命中的规则，只有候选规则需要时才读取命令行。
	nameMatched := make([]compiledProcessRule, 0, len(d.processRules))
	needsCmdline := false
	for _, rule := range d.processRules {
		if rule.comm == nil || rule.comm.MatchString(comm) {
			nameMatched = append(nameMatched, rule)
			needsCmdline = needsCmdline || rule.cmdline != nil
		}
	}
	if len(nameMatched) == 0 {
		return nil, nil
	}
	var cmdline string
	if needsCmdline {
		cmdline, _ = p.CmdlineWithContext(ctx)
	}
	matched := make([]compiledProcessRule, 0, len(d.processRules))
	for _, rule := range nameMatched {
		if rule.cmdline == nil || rule.cmdline.MatchString(cmdline) {
			matched = append(matched, rule)
		}
	}
	if len(matched) == 0 {
		return nil, nil
	}
	openFiles, err := p.OpenFilesWithContext(ctx)
	if err != nil {
		return nil, nil
	}
	var out []model.FileTarget
	for _, rule := range matched {
		count := 0
		for _, opened := range openFiles {
			path := strings.TrimSuffix(opened.Path, " (deleted)")
			if !filepath.IsAbs(path) || !rule.include.MatchString(path) || (rule.exclude != nil && rule.exclude.MatchString(path)) {
				continue
			}
			if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() {
				continue
			}
			attrs := clone(rule.rule.Attributes)
			attrs["process.pid"] = fmt.Sprintf("%d", p.Pid)
			attrs["process.name"] = comm
			attrs["process.command_line"] = cmdline
			out = append(out, model.FileTarget{Path: path, SourceType: "process", Rule: rule.rule.Name, StartAt: startAt(rule.rule.StartAt), Multiline: multiline(rule.rule.Multiline), Attributes: attrs, Extractors: rule.extractors, DropLevels: rule.dropLevels})
			count++
			if rule.rule.MaxFiles > 0 && count >= rule.rule.MaxFiles {
				break
			}
		}
	}
	return out, nil
}

func (d *Discovery) deduplicate(targets []model.FileTarget) []model.FileTarget {
	now := time.Now()
	cycle := make(map[string]struct{})
	unique := make([]model.FileTarget, 0, len(targets))
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, target := range targets {
		if _, ok := cycle[target.Path]; ok {
			continue
		}
		cycle[target.Path] = struct{}{}
		prepareResource(&target)
		unique = append(unique, target)
		if _, ok := d.seen[target.Path]; !ok {
			d.log.Info("discovered log file", "source", target.SourceType, "rule", target.Rule, "path", target.Path)
		}
		d.seen[target.Path] = now
	}
	for path, lastSeen := range d.seen {
		if now.Sub(lastSeen) > d.cacheTTL {
			delete(d.seen, path)
		}
	}
	return unique
}

func prepareResource(target *model.FileTarget) {
	resource := make(map[string]string, len(target.Attributes)+3)
	for k, v := range target.Attributes {
		resource[k] = v
	}
	resource["log.file.path"] = target.Path
	resource["log.source.type"] = target.SourceType
	resource["log.source.rule"] = target.Rule
	target.ResourceAttributes = resource
	keys := make([]string, 0, len(resource))
	for key := range resource {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var key strings.Builder
	for _, name := range keys {
		key.WriteString(name)
		key.WriteByte(0)
		key.WriteString(resource[name])
		key.WriteByte(0)
	}
	target.ResourceKey = key.String()
}

func discoverFiles(compiled compiledFileRule, sourceType string) []model.FileTarget {
	rule := compiled.rule
	excluded := make(map[string]struct{})
	for _, pattern := range rule.Exclude {
		matches, _ := filepath.Glob(pattern)
		for _, p := range matches {
			excluded[p] = struct{}{}
		}
	}
	var out []model.FileTarget
	for _, pattern := range rule.Include {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			if _, ok := excluded[path]; ok {
				continue
			}
			if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
				continue
			}
			attrs := clone(rule.Attributes)
			if sourceType == "container" {
				addContainerAttributes(attrs, path)
			}
			out = append(out, model.FileTarget{Path: path, SourceType: sourceType, Rule: rule.Name, StartAt: startAt(rule.StartAt), Format: rule.Format, Multiline: multiline(rule.Multiline), Attributes: attrs, Extractors: compiled.extractors, DropLevels: compiled.dropLevels})
		}
	}
	return out
}

func compileExtractors(values []config.AttributeExtractorConfig) ([]model.AttributeExtractor, error) {
	out := make([]model.AttributeExtractor, 0, len(values))
	for _, value := range values {
		pattern, err := regexp.Compile(value.Pattern)
		if err != nil {
			return nil, fmt.Errorf("compile attribute extractor %q: %w", value.Key, err)
		}
		out = append(out, model.AttributeExtractor{Key: value.Key, Pattern: pattern})
	}
	return out, nil
}

func dropLevels(values []string) map[string]struct{} {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[strings.ToUpper(value)] = struct{}{}
	}
	return out
}

func addContainerAttributes(attrs map[string]string, path string) {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	parts := strings.SplitN(name, "_", 3)
	if len(parts) != 3 {
		return
	}
	attrs["k8s.pod.name"] = parts[0]
	attrs["k8s.namespace.name"] = parts[1]
	if idx := strings.LastIndex(parts[2], "-"); idx > 0 {
		attrs["k8s.container.name"] = parts[2][:idx]
		attrs["container.id"] = parts[2][idx+1:]
	}
}
func optionalRegexp(s string) (*regexp.Regexp, error) {
	if s == "" {
		return nil, nil
	}
	return regexp.Compile(s)
}
func startAt(v string) string {
	if v == "" {
		return "end"
	}
	return v
}
func multiline(v config.MultilineConfig) model.Multiline {
	return model.Multiline{StartPattern: v.StartPattern, ContinuationPattern: v.ContinuationPattern, FlushAfter: v.FlushAfter.Duration}
}
func clone(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func errorsJoin(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(e.Error())
	}
	return fmt.Errorf("%s", b.String())
}
