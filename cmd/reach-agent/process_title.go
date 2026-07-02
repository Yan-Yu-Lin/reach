package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const defaultProcessTitle = "Reach-Agent"

type ProcessTitleSettings struct {
	Titles   []string
	Interval time.Duration
}

type ProcessTitleController struct {
	mu       sync.Mutex
	titles   []string
	interval time.Duration
	index    int
	updateCh chan struct{}
}

func defaultAgentConfigPath() string {
	if v := os.Getenv("REACH_AGENT_CONFIG"); v != "" {
		return v
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		homeCandidates := []string{
			filepath.Join(home, ".config", "reach", "agent.yaml"),
			filepath.Join(home, ".reach", "agent.yaml"),
		}
		if os.Geteuid() == 0 {
			candidates = append(candidates, "/etc/reach/agent.yaml")
			candidates = append(candidates, homeCandidates...)
		} else {
			candidates = append(candidates, homeCandidates...)
			candidates = append(candidates, "/etc/reach/agent.yaml")
		}
	} else {
		candidates = []string{"/etc/reach/agent.yaml"}
	}
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	if os.Geteuid() == 0 {
		return "/etc/reach/agent.yaml"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "reach", "agent.yaml")
	}
	return "/etc/reach/agent.yaml"
}

func isDiscoverableAgentConfig(path string) bool {
	if path == "" {
		return true
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	oldEnv := os.Getenv("REACH_AGENT_CONFIG")
	if oldEnv != "" {
		envAbs, err := filepath.Abs(oldEnv)
		if err == nil && envAbs == abs {
			return true
		}
	}
	candidates := []string{"/etc/reach/agent.yaml"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "reach", "agent.yaml"), filepath.Join(home, ".reach", "agent.yaml"))
	}
	for _, c := range candidates {
		cAbs, err := filepath.Abs(c)
		if err == nil && cAbs == abs {
			return true
		}
	}
	return false
}

func applyProcessTitles(ctx context.Context, cfg Config) *ProcessTitleController {
	c := &ProcessTitleController{updateCh: make(chan struct{}, 1)}
	c.Update(settingsFromAgentConfig(cfg))
	go c.run(ctx)
	return c
}

func settingsFromAgentConfig(cfg Config) ProcessTitleSettings {
	titles := append([]string(nil), cfg.ProcessTitles...)
	if len(titles) == 0 && cfg.ProcessTitle != "" {
		titles = []string{cfg.ProcessTitle}
	}
	return normalizeProcessTitleSettings(titles, cfg.rotateDur)
}

func settingsFromDesiredProcessTitleConfig(cfg *DesiredProcessTitleConfig, fallback Config) ProcessTitleSettings {
	if cfg == nil {
		return settingsFromAgentConfig(fallback)
	}
	titles := append([]string(nil), cfg.ProcessTitles...)
	if len(titles) == 0 && cfg.ProcessTitle != "" {
		titles = []string{cfg.ProcessTitle}
	}
	interval := fallback.rotateDur
	if strings.TrimSpace(cfg.RotateInterval) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(cfg.RotateInterval)); err == nil {
			interval = d
		}
	}
	return normalizeProcessTitleSettings(titles, interval)
}

func normalizeProcessTitleSettings(titles []string, interval time.Duration) ProcessTitleSettings {
	clean := make([]string, 0, len(titles))
	for _, title := range titles {
		title = cleanProcessTitle(title)
		if title != "" {
			clean = append(clean, title)
		}
	}
	if len(clean) == 0 {
		clean = []string{defaultProcessTitle}
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return ProcessTitleSettings{Titles: clean, Interval: interval}
}

func cleanProcessTitle(title string) string {
	title = strings.ReplaceAll(title, "\x00", " ")
	return strings.TrimSpace(title)
}

func truncateUTF8Bytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.ValidString(s[:max]) {
		_, size := utf8.DecodeLastRuneInString(s[:max])
		if size <= 0 || size > max {
			max--
		} else {
			max -= size
		}
	}
	return s[:max]
}

func (c *ProcessTitleController) Update(settings ProcessTitleSettings) {
	settings = normalizeProcessTitleSettings(settings.Titles, settings.Interval)
	first := settings.Titles[0]

	c.mu.Lock()
	if sameTitleSettings(c.titles, c.interval, settings) {
		c.mu.Unlock()
		return
	}
	c.titles = append([]string(nil), settings.Titles...)
	c.interval = settings.Interval
	c.index = 0
	c.mu.Unlock()

	setProcessTitle(first)
	c.notifyUpdate()
}

func (c *ProcessTitleController) run(ctx context.Context) {
	for {
		c.mu.Lock()
		interval := c.interval
		rotating := len(c.titles) > 1
		c.mu.Unlock()

		if !rotating {
			select {
			case <-ctx.Done():
				return
			case <-c.updateCh:
				continue
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			stopProcessTitleTimer(timer)
			return
		case <-c.updateCh:
			stopProcessTitleTimer(timer)
			continue
		case <-timer.C:
			c.advance()
		}
	}
}

func stopProcessTitleTimer(timer *time.Timer) {
	if timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (c *ProcessTitleController) advance() {
	c.mu.Lock()
	if len(c.titles) < 2 {
		c.mu.Unlock()
		return
	}
	c.index = (c.index + 1) % len(c.titles)
	title := c.titles[c.index]
	c.mu.Unlock()
	setProcessTitle(title)
}

func (c *ProcessTitleController) notifyUpdate() {
	select {
	case c.updateCh <- struct{}{}:
	default:
	}
}

func sameTitleSettings(titles []string, interval time.Duration, next ProcessTitleSettings) bool {
	if interval != next.Interval || len(titles) != len(next.Titles) {
		return false
	}
	for i := range titles {
		if titles[i] != next.Titles[i] {
			return false
		}
	}
	return true
}
