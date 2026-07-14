// skills.go - optional "skills" support: reusable, on-disk packets of
// task-specific best-practice instructions that ola can load at startup
// and hand to the model on demand. This is the exact same shape Claude's
// own skill system uses (one directory per skill, containing a SKILL.md
// file, e.g. /mnt/skills/public/<name>/SKILL.md).
//
// This stays entirely opt-in: unless a skills directory is configured (via
// --skills-dir or OLA_SKILLS_DIR), nothing in this file runs, no tool is
// added, and the model's session is completely unaffected - the same
// "only offer what actually works" principle used for run_command/
// web_search elsewhere in ola (see search.go, coding.go).
//
// Layout expected under each configured directory - two shapes are both
// supported, and can be mixed freely under the same --skills-dir:
//
//	<dir>/<skill-name>/SKILL.md             flat (ola's own convention,
//	                                         used throughout this file's
//	                                         tests)
//	<dir>/<category>/<skill-name>/SKILL.md  categorized (Anthropic's own
//	                                         layout, e.g.
//	                                         /mnt/skills/public/pptx,
//	                                         /mnt/skills/user/<name> - a
//	                                         --skills-dir pointed at
//	                                         their shared parent needs to
//	                                         see one level deeper than
//	                                         the flat case)
//
// A subdirectory only becomes a skill once it directly contains a
// SKILL.md; anything without one - at any depth up to maxSkillsScanDepth -
// is transparently descended into looking for skills nested deeper, so a
// --skills-dir can mix categorized and flat skills, or sit alongside
// unrelated folders, without any extra configuration. Once a directory IS
// recognized as a skill, its own subdirectories are treated purely as that
// skill's companion files (below) and are never searched for further,
// separately-listed skills.
//
// Symlinked directories are followed: a skill folder - or an entire
// category folder - that is itself a symlink (common when a skills
// directory is managed via dotfiles tooling such as GNU stow/chezmoi, or
// is a symlinked shared/mounted repo) is discovered exactly like a real
// directory, rather than silently skipped.
//
//	<dir>/.../<skill-name>/...   (optional companion files, at any
//	                              nesting depth: templates, reference
//	                              docs, helper scripts - e.g.
//	                              references/core-syntax.md or
//	                              assets/template.pptx - readable on
//	                              demand via read_skill's "file"
//	                              argument, given the path relative to
//	                              the skill's own folder with forward
//	                              slashes, regardless of host OS)
//
// SKILL.md may start with a minimal YAML-ish frontmatter block:
//
//	---
//	name: pptx
//	description: Use this skill whenever the user wants to create slides...
//	---
//	# rest of the instructions...
//
// name/description are deliberately NOT parsed as full YAML - ola stays a
// dependency-free single binary (see search.go's header for the same
// rationale) - just single-line "key: value" pairs between two "---"
// markers. If frontmatter is missing or incomplete, ola falls back to the
// directory's own name (for "name") and the first non-empty, non-heading
// line of body text (for "description").
//
// Multiple directories can be configured at once, comma-separated (same
// convention as --allow's binary list, e.g. "/mnt/skills/public,
// /mnt/skills/private"). Directories are scanned in the given order; the
// first skill found with a given name wins, and a same-named skill found
// again in a later directory is skipped with a warning rather than
// silently overwriting the first one.
//
// Only the short name+description pair for each skill is loaded into the
// system prompt up front (see buildSkillsPromptSection) - full SKILL.md
// content (and any companion files) is only pulled into context on demand
// via the read_skill tool, the same progressive-disclosure shape Claude's
// own skill system uses, so a session with many/large skills configured
// doesn't pay their full token cost unless the model actually needs one.
package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxSkillDescriptionChars caps how long a single skill's description is
// allowed to be once it lands in the system prompt - one skill's (possibly
// poorly trimmed, possibly copy-pasted) SKILL.md must not blow the prompt
// budget for every session that happens to have a skills directory
// configured, the same rationale as maxWebResultOutput in search.go.
const maxSkillDescriptionChars = 400

// maxSkillsScanDepth caps how many directory levels loadSkills will descend
// below each configured root while looking for a SKILL.md. Two levels
// covers the two real-world layouts described in the package doc comment:
// skills directly under the root, and skills grouped one level deeper
// under a category folder (Anthropic's own /mnt/skills/public/<name>-style
// layout). A little headroom beyond that keeps a mistakenly-broad
// --skills-dir (e.g. $HOME) from turning into an unbounded filesystem
// walk, and incidentally also bounds any symlink cycle, since symlinked
// directories are followed (see scanForSkillDirs).
const maxSkillsScanDepth = 4

// maxSkillFileListing caps how many companion-file paths toolReadSkill will
// enumerate for one skill (see listSkillFiles). Real-world skills like the
// bundled Anthropic ones can carry several dozen reference docs (e.g. a
// "references/" folder full of topic-specific .md files), so the cap only
// exists as a defensive backstop against a pathological skill folder
// (thousands of stray files) turning one read_skill call into an enormous
// tool result - it is not meant to bite for any realistically-sized skill.
const maxSkillFileListing = 500

// skillInfo describes one discovered skill: enough to list it in the
// system prompt (Name/Description) plus everything read_skill needs to
// fetch its full content or a companion file (Dir/SkillMDPath).
type skillInfo struct {
	Name        string
	Description string
	Dir         string // absolute path to the skill's own folder
	SkillMDPath string // absolute path to Dir/SKILL.md
}

// skillsConfig is the resolved result of --skills-dir/OLA_SKILLS_DIR: the
// directories that were searched, the skills actually found (name-sorted),
// and any non-fatal problems along the way (missing directory, a
// duplicate skill name, an unreadable SKILL.md). Warnings are collected
// rather than printed immediately so callers can decide where they belong
// (stderr, the output log, or both) - the same shape loadTimings uses in
// cmdAsk/cmdCoding.
type skillsConfig struct {
	Dirs     []string
	Skills   []skillInfo
	Warnings []string
}

func (c skillsConfig) enabled() bool { return len(c.Skills) > 0 }

// resolveSkillsDirs applies the same flag > env > default precedence used
// throughout ola (see resolveSearchConfig in search.go): an explicit
// --skills-dir wins, otherwise OLA_SKILLS_DIR, otherwise skills stay off
// entirely - unlike host/model/ctx, there is no sensible default directory
// to fall back to. Accepts a comma-separated list so more than one skills
// root can be combined in a single run (e.g. a shared/team directory plus
// a personal one).
func resolveSkillsDirs(flagVal string) []string {
	raw := flagVal
	if raw == "" {
		raw = os.Getenv("OLA_SKILLS_DIR")
	}
	if raw == "" {
		return nil
	}
	var dirs []string
	for _, d := range strings.Split(raw, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// loadSkills scans every directory in dirs (in order) for subdirectories
// containing a SKILL.md - at any depth, see scanForSkillDirs - parses each
// one, and returns the combined, name-sorted result. A directory that
// doesn't exist or can't be read is recorded as a warning, not a fatal
// error - a typo'd --skills-dir should degrade to "no skills loaded", not
// refuse to start the whole session.
func loadSkills(dirs []string) skillsConfig {
	cfg := skillsConfig{Dirs: dirs}
	seen := map[string]string{} // lower-cased skill name -> the skill dir that claimed it

	for _, dir := range dirs {
		skillDirs, err := scanForSkillDirs(dir)
		if err != nil {
			cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("skills-dir %s: อ่านไม่ได้ (%v) - ข้าม", dir, err))
			continue
		}
		for _, skillDir := range skillDirs {
			mdPath := filepath.Join(skillDir, "SKILL.md")
			name, desc, err := parseSkillMD(mdPath, filepath.Base(skillDir))
			if err != nil {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf("skill %s: อ่าน SKILL.md ไม่ได้ (%v) - ข้าม", skillDir, err))
				continue
			}
			key := strings.ToLower(name)
			if claimedBy, dup := seen[key]; dup {
				cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(
					"skill %q ที่ %s: ชื่อซ้ำกับ skill ที่โหลดจาก %s ไปแล้ว - ข้าม (directory ที่มาก่อนใน --skills-dir/OLA_SKILLS_DIR ชนะ)",
					name, skillDir, claimedBy))
				continue
			}
			seen[key] = skillDir

			absDir := skillDir
			if a, absErr := filepath.Abs(skillDir); absErr == nil {
				absDir = a
			}
			absMD := mdPath
			if a, absErr := filepath.Abs(mdPath); absErr == nil {
				absMD = a
			}
			cfg.Skills = append(cfg.Skills, skillInfo{
				Name: name, Description: desc, Dir: absDir, SkillMDPath: absMD,
			})
		}
	}

	sort.Slice(cfg.Skills, func(i, j int) bool {
		return strings.ToLower(cfg.Skills[i].Name) < strings.ToLower(cfg.Skills[j].Name)
	})
	return cfg
}

// scanForSkillDirs walks root looking for directories that directly
// contain a SKILL.md, at any depth up to maxSkillsScanDepth, and returns
// their paths in a deterministic (lexically sorted) order.
//
// This replaces a stricter, one-level-only os.ReadDir(dir) scan that
// missed two layouts real setups actually use:
//
//  1. Skills grouped under an intermediate category folder (see
//     maxSkillsScanDepth's doc comment) - a --skills-dir pointed at the
//     shared parent of such categories previously found nothing at all,
//     since none of the category folders themselves contain a SKILL.md.
//
//  2. A skill folder (or an entire category folder) that is itself a
//     symlink - e.g. from dotfiles tooling like GNU stow/chezmoi, or a
//     symlinked shared/mounted repo. The old code relied on
//     os.DirEntry.IsDir() from os.ReadDir, which reports the type of the
//     directory entry ITSELF and does not follow symlinks, so a symlinked
//     skill folder was silently invisible no matter how correctly it was
//     laid out inside. os.Stat, used here instead, does follow symlinks.
//
// Once a directory is found to contain a SKILL.md it is treated as a
// complete, terminal skill and is not searched any further: its own
// subdirectories are that skill's companion files (references/, assets/,
// etc. - see listSkillFiles, which handles enumerating those separately),
// not additional nested skills.
func scanForSkillDirs(root string) ([]string, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	var found []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // best-effort below the root: an unreadable subdirectory
			// contributes nothing, but shouldn't blank out sibling skills
			// found elsewhere under the same root.
		}
		for _, e := range entries {
			sub := filepath.Join(dir, e.Name())
			// os.Stat, not e.IsDir(), so a symlinked directory is followed
			// rather than skipped (see the doc comment above).
			info, statErr := os.Stat(sub)
			if statErr != nil || !info.IsDir() {
				continue
			}
			if _, mdErr := os.Stat(filepath.Join(sub, "SKILL.md")); mdErr == nil {
				found = append(found, sub)
				continue // terminal: don't also hunt for skills inside a skill
			}
			if depth < maxSkillsScanDepth {
				walk(sub, depth+1)
			}
		}
	}
	walk(root, 1)
	sort.Strings(found)
	return found, nil
}

// parseSkillMD extracts a (name, description) pair from a SKILL.md file.
// It understands a minimal "---\nkey: value\n---" frontmatter block
// (name/description keys, single-line values only - see the package doc
// comment for why this isn't full YAML) and falls back to fallbackName
// (the skill's directory name) plus the first non-empty, non-heading body
// line when frontmatter is absent or incomplete.
func parseSkillMD(path, fallbackName string) (name, description string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(string(data), "\n")

	bodyStart := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		fm := map[string]string{}
		i := 1
		for ; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if line == "---" {
				i++
				break
			}
			if k, v, ok := strings.Cut(line, ":"); ok {
				fm[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
		bodyStart = i
		name = fm["name"]
		description = fm["description"]
	}

	if name == "" {
		name = fallbackName
		// A leading "# Heading" in the body reads better than a raw
		// directory name, if frontmatter didn't already supply a name.
		for _, l := range lines[min(bodyStart, len(lines)):] {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			if h := strings.TrimLeft(t, "#"); h != t {
				if title := strings.TrimSpace(h); title != "" {
					name = title
				}
			}
			break
		}
	}

	if description == "" {
		for _, l := range lines[min(bodyStart, len(lines)):] {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			description = t
			break
		}
	}

	name = strings.TrimSpace(name)
	description = truncateRunes(strings.TrimSpace(description), maxSkillDescriptionChars)
	if description == "" {
		description = "(ไม่มีคำอธิบายใน SKILL.md)"
	}
	return name, description, nil
}

// truncateRunes is a small, rune-safe cap used only for cosmetic prompt
// sizing here - unlike truncateUTF8Bytes in main.go (which exists
// specifically for ntfy's hard byte-limit safety net), there is no
// byte-budget correctness requirement for a system-prompt description, so
// a simple rune count is sufficient and keeps multi-byte Thai text intact.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// buildSkillsPromptSection renders the "# AVAILABLE SKILLS" block appended
// to the system prompt when skillsCfg.enabled(). Listing name+description
// only (not full content) keeps the base prompt cheap no matter how many
// skills are configured - see the package doc comment on
// progressive disclosure via read_skill.
func buildSkillsPromptSection(skills []skillInfo) string {
	var b strings.Builder
	b.WriteString("\n\n# AVAILABLE SKILLS\n")
	b.WriteString("ola was started with a skills directory configured. Each entry below is a\n")
	b.WriteString("folder of best-practice instructions for a specific task/document type -\n")
	b.WriteString("read the relevant one(s) BEFORE starting matching work, via the read_skill\n")
	b.WriteString("tool (same idea as read_file before editing: don't guess what a skill says,\n")
	b.WriteString("read it first). Several skills may apply to one task; the mapping from task\n")
	b.WriteString("to skill isn't always obvious from the name alone, so check the description\n")
	b.WriteString("below rather than skipping a skill that might apply.\n\n")
	for _, s := range skills {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
}

// ─────────────────────────────────────────────────────────────────
// read_skill tool
// ─────────────────────────────────────────────────────────────────

var readSkillTool = ollamaTool{
	Type: "function",
	Function: ollamaToolFunction{
		Name: "read_skill",
		Description: "Read the full SKILL.md instructions for one of the skills listed in the " +
			"AVAILABLE SKILLS section of the system prompt, or (with the optional \"file\" argument) " +
			"a companion file inside that same skill's own folder (e.g. a template or reference doc " +
			"the SKILL.md points to). The default call (no \"file\") also returns a list of every " +
			"companion file that skill has, at any nesting depth (e.g. \"references/core-syntax.md\", " +
			"\"assets/template.pptx\") - pass one of those exact paths as \"file\" on a follow-up call to " +
			"read it. Only present when ola was started with a skills directory configured " +
			"(--skills-dir/OLA_SKILLS_DIR) and at least one skill was found in it.",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"skill": map[string]interface{}{
					"type":        "string",
					"description": "Exact skill name as listed in AVAILABLE SKILLS.",
				},
				"file": map[string]interface{}{
					"type": "string",
					"description": "Optional path to a companion file, relative to that skill's own folder and using forward " +
						"slashes (e.g. \"references/core-syntax.md\"), to read it instead of SKILL.md itself. Use one of the " +
						"exact paths returned by a prior call to this skill without \"file\".",
				},
			},
			"required": []string{"skill"},
		},
	},
}

// listSkillFiles recursively walks a skill's own directory and returns
// every companion file inside it (SKILL.md itself excluded), as paths
// relative to dir with forward slashes - i.e. exactly the string the model
// should pass back as read_skill's "file" argument to fetch that file,
// unchanged, no matter what OS ola is running on.
//
// This has to be a recursive walk rather than a single os.ReadDir (which
// is all toolReadSkill used to do): real skills commonly nest companion
// content a level or two deep - a "references/" folder full of topic docs
// (see e.g. the bundled slidev skill's references/core-syntax.md,
// references/diagram-mermaid.md, and dozens more alongside it), a
// "scripts/" or "assets/" folder, etc. A shallow listing would only ever
// report the top-level entry "references" itself - which isn't something
// read_skill can actually return content for (it's a directory, not a
// file; toolReadSkill's IsDir check below would reject it) - leaving the
// model no way to discover the real, fetchable paths underneath it
// without already knowing them from SKILL.md's own prose.
//
// Only files are listed, not directories: a directory name adds no
// information the model can act on (it can't be "read"), and correctly
// nested file paths already make clear where things live.
func listSkillFiles(dir string) []string {
	var files []string
	_ = fs.WalkDir(os.DirFS(dir), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: one unreadable entry shouldn't blank out the whole listing
		}
		if path == "." || d.IsDir() {
			return nil
		}
		if path == "SKILL.md" {
			return nil
		}
		files = append(files, filepath.ToSlash(path))
		if len(files) >= maxSkillFileListing {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files
}

// findSkill looks up a skill by name, case-insensitively (models don't
// always reproduce exact casing from the system prompt back verbatim).
func findSkill(skills []skillInfo, name string) (skillInfo, bool) {
	name = strings.TrimSpace(name)
	for _, s := range skills {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return skillInfo{}, false
}

// toolReadSkill dispatches the read_skill tool call: full SKILL.md content
// (plus a listing of any sibling files, so the model knows what else is
// available without an extra round trip) by default, or one specific
// companion file when "file" is given. Companion-file access is sandboxed
// to that skill's own directory via sandboxedPathIn - same rejection rule
// as the regular file tools' sandboxedPath, just rooted at the skill's own
// folder instead of the current working directory, so "file" can't be
// used to read arbitrary paths elsewhere on disk.
func toolReadSkill(args map[string]interface{}, skills []skillInfo) (string, error) {
	skillName, _ := args["skill"].(string)
	if skillName == "" {
		return "", fmt.Errorf("ต้องระบุ skill")
	}
	s, ok := findSkill(skills, skillName)
	if !ok {
		names := make([]string, len(skills))
		for i, sk := range skills {
			names[i] = sk.Name
		}
		return "", fmt.Errorf("ไม่พบ skill %q - ที่มีอยู่: %s", skillName, strings.Join(names, ", "))
	}

	if file, _ := args["file"].(string); file != "" {
		full, err := sandboxedPathIn(s.Dir, file)
		if err != nil {
			return "", err
		}
		info, statErr := os.Stat(full)
		if statErr != nil {
			return "", fmt.Errorf("ไม่พบไฟล์ %s ใน skill %q", file, s.Name)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s เป็น directory ไม่ใช่ไฟล์", file)
		}
		if looksBinaryFile(full, info) {
			return "", fmt.Errorf("%s ดูเหมือนเป็น binary file - ไม่รองรับการอ่านเป็นข้อความ", file)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return "", fmt.Errorf("อ่านไฟล์ %s ของ skill %q ไม่ได้: %v", file, s.Name, err)
		}
		return string(data), nil
	}

	data, err := os.ReadFile(s.SkillMDPath)
	if err != nil {
		return "", fmt.Errorf("อ่าน SKILL.md ของ skill %q ไม่ได้: %v", s.Name, err)
	}

	sibling := listSkillFiles(s.Dir)

	result := string(data)
	if len(sibling) > 0 {
		note := fmt.Sprintf("\n\n(ไฟล์อื่นในโฟลเดอร์ของ skill นี้ที่อ่านเพิ่มได้ผ่าน read_skill(skill=%q, file=...): %s)",
			s.Name, strings.Join(sibling, ", "))
		if len(sibling) >= maxSkillFileListing {
			note += fmt.Sprintf(" (แสดง %d ไฟล์แรก อาจมีมากกว่านี้)", maxSkillFileListing)
		}
		result += note
	}
	return result, nil
}
