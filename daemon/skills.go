package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---- skill catalog --------------------------------------------------------

var (
	skillCacheLock  sync.Mutex
	skillCacheMtime time.Time
	skillCacheItems []skillItem
)

func skillCatalog() []skillItem {
	skillCacheLock.Lock()
	defer skillCacheLock.Unlock()
	st, err := os.Stat(SKILLS_DIR)
	if err != nil {
		return nil
	}
	mt := st.ModTime()
	entries, _ := os.ReadDir(SKILLS_DIR)
	for _, e := range entries {
		sm := filepath.Join(SKILLS_DIR, e.Name(), "SKILL.md")
		if s, err := os.Stat(sm); err == nil && s.ModTime().After(mt) {
			mt = s.ModTime()
		}
	}
	if !mt.IsZero() && mt.Equal(skillCacheMtime) {
		return skillCacheItems
	}
	items := []skillItem{}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sm := filepath.Join(SKILLS_DIR, e.Name(), "SKILL.md")
		b, err := os.ReadFile(sm)
		if err != nil {
			continue
		}
		txt := string(b)
		name := e.Name()
		desc := ""
		body := txt
		if strings.HasPrefix(txt, "---") {
			end := strings.Index(txt[3:], "\n---")
			if end > 0 {
				fm := txt[3 : 3+end]
				body = txt[3+end+4:]
				for _, ln := range strings.Split(fm, "\n") {
					i := strings.Index(ln, ":")
					if i < 0 {
						continue
					}
					k := strings.TrimSpace(ln[:i])
					v := strings.TrimSpace(ln[i+1:])
					if k == "name" && v != "" {
						name = v
					} else if k == "description" && v != "" {
						desc = v
					}
				}
			}
		}
		if desc == "" {
			for _, ln := range strings.Split(body, "\n") {
				ln = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "#"))
				if ln != "" {
					desc = ln
					break
				}
			}
		}
		items = append(items, skillItem{
			Name: name, Description: desc,
			Path: "/skills/" + e.Name() + "/SKILL.md",
		})
	}
	skillCacheMtime = mt
	skillCacheItems = items
	return items
}

func composeSystemPrompt(g string) string {
	var parts []string
	if b, err := os.ReadFile(filepath.Join(HERE, "prompts", "global.md")); err == nil {
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	if b, err := os.ReadFile(filepath.Join(vol(g), "prompt.md")); err == nil {
		parts = append(parts, strings.TrimRight(string(b), "\n"))
	}
	var enabled []string
	if b, err := os.ReadFile(filepath.Join(vol(g), ".cs", "config.json")); err == nil {
		var cfg map[string]any
		if json.Unmarshal(b, &cfg) == nil {
			if arr, ok := cfg["skills"].([]any); ok {
				for _, v := range arr {
					if s, ok := v.(string); ok {
						enabled = append(enabled, s)
					}
				}
			}
		}
	}
	if len(enabled) > 0 {
		cat := map[string]skillItem{}
		for _, it := range skillCatalog() {
			cat[it.Name] = it
		}
		lines := []string{
			"## Available skills",
			"These are curated for this group. Load full content via your Read tool when relevant; descriptions below are deliberately terse.",
		}
		any := false
		for _, nm := range enabled {
			it, ok := cat[nm]
			if !ok {
				continue
			}
			any = true
			lines = append(lines, fmt.Sprintf("- **%s**: %s — path: %s", it.Name, it.Description, it.Path))
		}
		if any {
			parts = append(parts, strings.Join(lines, "\n"))
		}
	}
	parts = append(parts,
		"## Memory\n"+
			"Your persistent memory namespace is at /workspace/memory/. "+
			"Read MEMORY.md first for the index; create or update files under /workspace/memory/ "+
			"to persist facts across turns. Memory survives /clear.")
	out := []string{}
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// ---- skills cmd -----------------------------------------------------------

var skillNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func skillListCmd(req skillListReq) skillsResp {
	items := skillCatalog()
	enabled := map[string]bool{}
	if req.Group != "" {
		p := filepath.Join(vol(req.Group), ".cs", "config.json")
		if b, err := os.ReadFile(p); err == nil {
			var cfg map[string]any
			if json.Unmarshal(b, &cfg) == nil {
				if arr, ok := cfg["skills"].([]any); ok {
					for _, v := range arr {
						if s, ok := v.(string); ok {
							enabled[s] = true
						}
					}
				}
			}
		}
	}
	out := make([]skillItem, len(items))
	for i, it := range items {
		it.Enabled = enabled[it.Name]
		out[i] = it
	}
	return skillsResp{BaseResp: baseResp{OK: true}, Skills: out}
}

func skillNewCmd(req skillNewReq) skillNewResp {
	name := strings.TrimSpace(req.Name)
	if !skillNameRE.MatchString(name) {
		return skillNewResp{BaseResp: errResp("name must match [a-z0-9][a-z0-9_-]{0,63}")}
	}
	d := filepath.Join(SKILLS_DIR, name)
	p := filepath.Join(d, "SKILL.md")
	if _, err := os.Stat(p); err == nil {
		return skillNewResp{BaseResp: errResp("skills/" + name + "/SKILL.md already exists")}
	}
	_ = os.MkdirAll(d, 0o755)
	body := fmt.Sprintf("---\nname: %s\ndescription: TODO one-line description.\n---\n# %s\n\nReplace this body with the skill's full instructions.\n", name, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		return skillNewResp{BaseResp: errResp(err.Error())}
	}
	skillCacheLock.Lock()
	skillCacheMtime = time.Time{}
	skillCacheLock.Unlock()
	rel, _ := filepath.Rel(HERE, p)
	return skillNewResp{BaseResp: baseResp{OK: true}, Path: rel}
}

// skillWriteCmd creates or overwrites a skill's SKILL.md with full content.
// This is the ctl-plane replacement for main's podman-era rw /skills mount:
// under the firecracker runtime /skills in the guest is a tarball copy, so
// authoring goes through the daemon (which owns the host skills/ dir) and
// reaches peers on their next spawn. Size-capped: the ctl plane is driven by
// a tier-3 agent, and "fill the host disk one JSON line at a time" shouldn't
// be in its blast radius.
func skillWriteCmd(name, content string) skillNewResp {
	name = strings.TrimSpace(name)
	if !skillNameRE.MatchString(name) {
		return skillNewResp{BaseResp: errResp("name must match [a-z0-9][a-z0-9_-]{0,63}")}
	}
	if len(content) == 0 || len(content) > 256*1024 {
		return skillNewResp{BaseResp: errResp("content must be 1B..256KB")}
	}
	d := filepath.Join(SKILLS_DIR, name)
	p := filepath.Join(d, "SKILL.md")
	if err := os.MkdirAll(d, 0o755); err != nil {
		return skillNewResp{BaseResp: errResp(err.Error())}
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		return skillNewResp{BaseResp: errResp(err.Error())}
	}
	skillCacheLock.Lock()
	skillCacheMtime = time.Time{}
	skillCacheLock.Unlock()
	rel, _ := filepath.Rel(HERE, p)
	return skillNewResp{BaseResp: baseResp{OK: true}, Path: rel}
}

func skillReadCmd(req skillReadReq) skillReadResp {
	name := strings.TrimSpace(req.Name)
	if !skillNameRE.MatchString(name) {
		return skillReadResp{BaseResp: errResp("invalid skill name")}
	}
	p := filepath.Join(SKILLS_DIR, name, "SKILL.md")
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return skillReadResp{BaseResp: errResp("no such skill: " + name)}
		}
		return skillReadResp{BaseResp: errResp(err.Error())}
	}
	return skillReadResp{BaseResp: baseResp{OK: true}, Name: name, Content: string(b)}
}
