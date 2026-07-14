package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// ─────────────────────────────────────────────────────────────────
// parseSkillMD
// ─────────────────────────────────────────────────────────────────

func writeSkillMD(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "SKILL.md")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParseSkillMDReadsFrontmatter confirms the primary, intended path:
// an explicit name/description in a "---" frontmatter block is used
// as-is, with no fallback guessing needed at all.
func TestParseSkillMDReadsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "---\nname: pptx\ndescription: Use this whenever the user wants slides.\n---\n# PPTX\nBody text here.\n")

	name, desc, err := parseSkillMD(path, "fallback-dir-name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "pptx" {
		t.Fatalf("expected name %q from frontmatter, got %q", "pptx", name)
	}
	if desc != "Use this whenever the user wants slides." {
		t.Fatalf("expected description from frontmatter, got %q", desc)
	}
}

// TestParseSkillMDFallsBackToHeadingAndFirstLine confirms a SKILL.md with
// no frontmatter at all still yields a usable name (from its leading "#"
// heading) and description (the first substantive body line) instead of
// erroring out - most hand-written skills won't bother with frontmatter.
func TestParseSkillMDFallsBackToHeadingAndFirstLine(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "# My Great Skill\n\nThis is what it does for you.\nMore detail on a second line.\n")

	name, desc, err := parseSkillMD(path, "my-great-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "My Great Skill" {
		t.Fatalf("expected name derived from the leading heading, got %q", name)
	}
	if desc != "This is what it does for you." {
		t.Fatalf("expected description to be the first substantive body line, got %q", desc)
	}
}

// TestParseSkillMDFallsBackToDirNameWithoutHeading confirms a SKILL.md
// that starts directly with prose (no frontmatter, no leading heading)
// falls all the way back to the skill's own directory name, rather than
// misreading the first prose line as a title.
func TestParseSkillMDFallsBackToDirNameWithoutHeading(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "Just a plain description with no heading above it.\n")

	name, desc, err := parseSkillMD(path, "plain-skill")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "plain-skill" {
		t.Fatalf("expected name to fall back to the directory name, got %q", name)
	}
	if desc != "Just a plain description with no heading above it." {
		t.Fatalf("expected the first line to be used as description, got %q", desc)
	}
}

// TestParseSkillMDPartialFrontmatterFillsInMissingField confirms
// frontmatter with only "name:" set still recovers a description from the
// body, rather than leaving it as the "(no description)" placeholder just
// because frontmatter existed at all.
func TestParseSkillMDPartialFrontmatterFillsInMissingField(t *testing.T) {
	dir := t.TempDir()
	path := writeSkillMD(t, dir, "---\nname: partial\n---\n# Heading (ignored - name already set)\nRecovered description line.\n")

	name, desc, err := parseSkillMD(path, "fallback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if name != "partial" {
		t.Fatalf("expected frontmatter name to win, got %q", name)
	}
	if desc != "Recovered description line." {
		t.Fatalf("expected description to be recovered from the body, got %q", desc)
	}
}

// TestParseSkillMDTruncatesLongDescription confirms a single skill's
// description can't blow the system-prompt budget for every session that
// happens to have a skills directory configured - see
// maxSkillDescriptionChars.
func TestParseSkillMDTruncatesLongDescription(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", maxSkillDescriptionChars+200)
	path := writeSkillMD(t, dir, "---\nname: verbose\ndescription: "+long+"\n---\n")

	_, desc, err := parseSkillMD(path, "fallback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	descRunes := []rune(desc)
	if len(descRunes) != maxSkillDescriptionChars+1 { // +1 for the trailing "…" marker
		t.Fatalf("expected description truncated to %d runes + ellipsis, got %d runes: %q", maxSkillDescriptionChars, len(descRunes), desc)
	}
	if !strings.HasSuffix(desc, "…") {
		t.Fatalf("expected a truncation marker at the end, got %q", desc)
	}
}

// TestParseSkillMDMissingFile confirms a missing SKILL.md surfaces as a
// normal Go error rather than panicking or silently returning empty
// strings - loadSkills relies on this to turn it into a warning.
func TestParseSkillMDMissingFile(t *testing.T) {
	if _, _, err := parseSkillMD(filepath.Join(t.TempDir(), "nope", "SKILL.md"), "fallback"); err == nil {
		t.Fatal("expected an error for a missing SKILL.md")
	}
}

// ─────────────────────────────────────────────────────────────────
// resolveSkillsDirs
// ─────────────────────────────────────────────────────────────────

// TestResolveSkillsDirsFlagOverridesEnv confirms the same flag > env >
// default precedence used throughout ola (see resolveSearchConfig).
func TestResolveSkillsDirsFlagOverridesEnv(t *testing.T) {
	t.Setenv("OLA_SKILLS_DIR", "/from/env")
	got := resolveSkillsDirs("/from/flag")
	if len(got) != 1 || got[0] != "/from/flag" {
		t.Fatalf("expected flag to win over env, got %v", got)
	}
}

// TestResolveSkillsDirsFallsBackToEnv confirms OLA_SKILLS_DIR is used when
// no --skills-dir flag was given.
func TestResolveSkillsDirsFallsBackToEnv(t *testing.T) {
	t.Setenv("OLA_SKILLS_DIR", "/from/env")
	got := resolveSkillsDirs("")
	if len(got) != 1 || got[0] != "/from/env" {
		t.Fatalf("expected env value, got %v", got)
	}
}

// TestResolveSkillsDirsSplitsAndTrimsCommaList mirrors --allow's
// comma-separated convention: multiple directories, extra whitespace and
// empty segments handled gracefully.
func TestResolveSkillsDirsSplitsAndTrimsCommaList(t *testing.T) {
	got := resolveSkillsDirs(" /a/skills , /b/skills ,,/c/skills")
	want := []string{"/a/skills", "/b/skills", "/c/skills"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segment %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestResolveSkillsDirsDefaultsToNil confirms skills stay off entirely
// (nil, not an empty-but-non-nil slice someone could accidentally treat as
// "configured") when neither the flag nor the env var is set - there is
// deliberately no default directory, unlike host/model/ctx.
func TestResolveSkillsDirsDefaultsToNil(t *testing.T) {
	t.Setenv("OLA_SKILLS_DIR", "")
	if got := resolveSkillsDirs(""); got != nil {
		t.Fatalf("expected nil when nothing is configured, got %v", got)
	}
}

// ─────────────────────────────────────────────────────────────────
// loadSkills
// ─────────────────────────────────────────────────────────────────

// TestLoadSkillsScansSubdirsAndSkipsNonSkillFolders confirms only
// immediate subdirectories that actually contain a SKILL.md become
// skills - a stray subdirectory without one (e.g. some unrelated folder
// living alongside a skills root) is silently ignored, not an error.
func TestLoadSkillsScansSubdirsAndSkipsNonSkillFolders(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: Slides.\n---\n")
	mustMkdirSkill(t, root, "docx", "---\nname: docx\ndescription: Word docs.\n---\n")
	if err := os.MkdirAll(filepath.Join(root, "not-a-skill"), 0755); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 2 {
		t.Fatalf("expected exactly 2 skills, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
	// name-sorted: docx before pptx
	if cfg.Skills[0].Name != "docx" || cfg.Skills[1].Name != "pptx" {
		t.Fatalf("expected name-sorted [docx, pptx], got [%s, %s]", cfg.Skills[0].Name, cfg.Skills[1].Name)
	}
	if !cfg.enabled() {
		t.Fatal("expected skillsConfig.enabled() to be true when skills were found")
	}
}

// TestLoadSkillsFirstDirWinsOnDuplicateName confirms a skill name found in
// more than one configured directory keeps the FIRST directory's version
// (matching the documented --skills-dir/OLA_SKILLS_DIR precedence: earlier
// directories win) and records a warning about the shadowed duplicate,
// rather than silently overwriting it or erroring out the whole run.
func TestLoadSkillsFirstDirWinsOnDuplicateName(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	mustMkdirSkill(t, dirA, "shared", "---\nname: shared\ndescription: version A (should win).\n---\n")
	mustMkdirSkill(t, dirB, "shared", "---\nname: shared\ndescription: version B (should be skipped).\n---\n")

	cfg := loadSkills([]string{dirA, dirB})
	if len(cfg.Skills) != 1 {
		t.Fatalf("expected exactly 1 skill after dedup, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
	if cfg.Skills[0].Description != "version A (should win)." {
		t.Fatalf("expected the first directory's version to win, got %q", cfg.Skills[0].Description)
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "ชื่อซ้ำ") {
		t.Fatalf("expected exactly one duplicate-name warning, got: %v", cfg.Warnings)
	}
}

// TestLoadSkillsWarnsButDoesNotFailOnMissingDirectory confirms a typo'd or
// nonexistent --skills-dir/OLA_SKILLS_DIR entry degrades to "no skills
// from that directory" plus a warning, rather than making the whole
// session (ask/coding) refuse to start.
func TestLoadSkillsWarnsButDoesNotFailOnMissingDirectory(t *testing.T) {
	cfg := loadSkills([]string{filepath.Join(t.TempDir(), "does-not-exist")})
	if cfg.enabled() {
		t.Fatal("expected no skills to be found")
	}
	if len(cfg.Warnings) != 1 || !strings.Contains(cfg.Warnings[0], "อ่านไม่ได้") {
		t.Fatalf("expected exactly one unreadable-directory warning, got: %v", cfg.Warnings)
	}
}

// TestLoadSkillsCombinesMultipleDirectories confirms distinct skills
// across several configured directories are all loaded together, not just
// the first directory - the comma-separated list is additive.
func TestLoadSkillsCombinesMultipleDirectories(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	mustMkdirSkill(t, dirA, "alpha", "---\nname: alpha\ndescription: from dir A.\n---\n")
	mustMkdirSkill(t, dirB, "beta", "---\nname: beta\ndescription: from dir B.\n---\n")

	cfg := loadSkills([]string{dirA, dirB})
	if len(cfg.Skills) != 2 {
		t.Fatalf("expected 2 skills combined from both directories, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
}

// TestLoadSkillsFindsCategoryNestedSkills is the regression test for the
// "flat scan only" bug: --skills-dir/OLA_SKILLS_DIR previously only ever
// looked one level below the configured directory
// (<dir>/<skill-name>/SKILL.md), so a directory laid out the way
// Anthropic's own Claude products organize skills - grouped one level
// deeper under a category folder, e.g. <dir>/public/pptx/SKILL.md,
// <dir>/user/rust-tokio-secure-systems/SKILL.md - was invisible: none of
// the category folders themselves ("public", "user") contain a SKILL.md,
// so the old scan found nothing at all and silently reported zero skills.
// This confirms skills nested under a category directory are now found,
// and categorized/flat layouts can be mixed under the same root.
func TestLoadSkillsFindsCategoryNestedSkills(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, filepath.Join(root, "public"), "pptx", "---\nname: pptx\ndescription: Slides.\n---\n")
	mustMkdirSkill(t, filepath.Join(root, "user"), "rust-tokio-secure-systems", "---\nname: rust-tokio-secure-systems\ndescription: Rust backends.\n---\n")
	// A flat (non-categorized) skill directly under the same root must
	// still be found too - the two layouts are not mutually exclusive.
	mustMkdirSkill(t, root, "find-skills", "---\nname: find-skills\ndescription: Meta skill.\n---\n")

	cfg := loadSkills([]string{root})
	if len(cfg.Warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", cfg.Warnings)
	}
	got := map[string]bool{}
	for _, s := range cfg.Skills {
		got[s.Name] = true
	}
	for _, want := range []string{"pptx", "rust-tokio-secure-systems", "find-skills"} {
		if !got[want] {
			t.Fatalf("expected skill %q to be found (category-nested or flat), got: %+v", want, cfg.Skills)
		}
	}
	if len(cfg.Skills) != 3 {
		t.Fatalf("expected exactly 3 skills total, got %d: %+v", len(cfg.Skills), cfg.Skills)
	}
}

// TestLoadSkillsFindsSymlinkedSkillDir is the regression test for the
// symlink-invisibility bug: os.ReadDir's fs.DirEntry.IsDir() reports the
// type of the directory entry ITSELF and does not follow symlinks, so a
// skill folder that is itself a symlink (a common shape for skills
// directories managed via dotfiles tooling like GNU stow/chezmoi, or a
// symlinked shared/mounted repo) was silently skipped by the old
// !e.IsDir() { continue } check, even though its target was a perfectly
// well-formed skill directory with its own SKILL.md. This confirms a
// symlinked skill directory is now discovered exactly like a real one.
func TestLoadSkillsFindsSymlinkedSkillDir(t *testing.T) {
	root := t.TempDir()
	realDir := t.TempDir()
	mustMkdirSkill(t, realDir, "slidev", "---\nname: slidev\ndescription: Build Slidev decks.\n---\n")

	link := filepath.Join(root, "slidev")
	if err := os.Symlink(filepath.Join(realDir, "slidev"), link); err != nil {
		t.Skipf("symlinks not supported on this filesystem: %v", err)
	}

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "slidev" {
		t.Fatalf("expected the symlinked skill directory to be found, got %+v (warnings: %v)", cfg.Skills, cfg.Warnings)
	}
}

// TestLoadSkillsStopsAtFirstSkillLevelFound confirms a directory that is
// itself already recognized as a skill (it has its own SKILL.md) is never
// searched further for additional, separately-listed skills nested inside
// it - a companion folder like "references/" is that skill's own material
// (see listSkillFiles), not a place to go looking for more top-level
// skills, even if - hypothetically - a file happened to be named SKILL.md
// somewhere further down inside it.
func TestLoadSkillsStopsAtFirstSkillLevelFound(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "slidev", "---\nname: slidev\ndescription: Build Slidev decks.\n---\n")
	// A stray, coincidentally-named SKILL.md living inside slidev's own
	// references/ folder must not be picked up as a second skill.
	nested := filepath.Join(root, "slidev", "references", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(nested), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nested, []byte("---\nname: should-not-appear\ndescription: d.\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != "slidev" {
		t.Fatalf("expected exactly 1 skill (slidev) with nested content ignored, got %+v", cfg.Skills)
	}
}

// TestLoadSkillsRespectsMaxScanDepth confirms a skill buried deeper than
// maxSkillsScanDepth allows is not found - the depth cap exists precisely
// to keep a mistakenly-broad --skills-dir (e.g. accidentally pointed at a
// huge or unrelated directory tree) from turning into an unbounded
// filesystem walk, so this documents that the cap actually bites.
func TestLoadSkillsRespectsMaxScanDepth(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i < maxSkillsScanDepth+2; i++ {
		deep = filepath.Join(deep, "level")
	}
	mustMkdirSkill(t, deep, "too-deep", "---\nname: too-deep\ndescription: d.\n---\n")

	cfg := loadSkills([]string{root})
	if len(cfg.Skills) != 0 {
		t.Fatalf("expected a skill beyond maxSkillsScanDepth to be out of reach, got: %+v", cfg.Skills)
	}
}

func mustMkdirSkill(t *testing.T, root, name, skillMD string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0644); err != nil {
		t.Fatal(err)
	}
}

// ─────────────────────────────────────────────────────────────────
// buildSkillsPromptSection
// ─────────────────────────────────────────────────────────────────

// TestBuildSkillsPromptSectionListsEveryNameAndDescription confirms the
// system-prompt injection is a genuine listing of what was loaded, not
// just a static header - the model has to see the actual names/
// descriptions to pick the right skill.
func TestBuildSkillsPromptSectionListsEveryNameAndDescription(t *testing.T) {
	section := buildSkillsPromptSection([]skillInfo{
		{Name: "pptx", Description: "Slide decks."},
		{Name: "docx", Description: "Word documents."},
	})
	if !strings.Contains(section, "AVAILABLE SKILLS") {
		t.Fatalf("expected a clearly-labeled section header, got:\n%s", section)
	}
	if !strings.Contains(section, "pptx: Slide decks.") {
		t.Fatalf("expected the pptx entry, got:\n%s", section)
	}
	if !strings.Contains(section, "docx: Word documents.") {
		t.Fatalf("expected the docx entry, got:\n%s", section)
	}
}

// ─────────────────────────────────────────────────────────────────
// toolReadSkill
// ─────────────────────────────────────────────────────────────────

// TestToolReadSkillReturnsFullContentAndSiblingListing confirms the
// default (no "file" argument) call returns the complete SKILL.md body -
// not just the truncated description used in the system prompt - plus a
// hint about any companion files so the model knows to ask for them by
// name instead of guessing paths.
func TestToolReadSkillReturnsFullContentAndSiblingListing(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "pptx")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		t.Fatal(err)
	}
	fullBody := "---\nname: pptx\ndescription: short.\n---\n# Full instructions\nLots of detail that would be too long for the system prompt.\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(fullBody), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "template.pptx.md"), []byte("template contents"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	result, err := toolReadSkill(map[string]interface{}{"skill": "pptx"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Lots of detail that would be too long for the system prompt.") {
		t.Fatalf("expected the full SKILL.md body, got: %s", result)
	}
	if !strings.Contains(result, "template.pptx.md") {
		t.Fatalf("expected the sibling file to be mentioned so the model knows it exists, got: %s", result)
	}
}

// TestToolReadSkillListsNestedReferenceFilesRecursively is the regression
// test for the shallow-listing bug: real skills commonly nest companion
// docs a level deep (a "references/" folder full of topic-specific .md
// files is the exact shape of Anthropic's own bundled skills, e.g. slidev's
// references/core-syntax.md, references/diagram-mermaid.md, and dozens
// more alongside them). A listing that only reports the top-level entry
// "references" itself is useless to the model - that path isn't a file
// read_skill can return content for - and gives no way to discover the
// real, fetchable paths underneath without already knowing them. This
// confirms the sibling listing walks into subdirectories and reports full,
// slash-joined relative paths instead.
func TestToolReadSkillListsNestedReferenceFilesRecursively(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "slidev")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: slidev\ndescription: Build Slidev decks.\n---\nBody.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "README.md"), []byte("readme"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "core-syntax.md"), []byte("core"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "diagram-mermaid.md"), []byte("mermaid"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	result, err := toolReadSkill(map[string]interface{}{"skill": "slidev"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, want := range []string{"references/core-syntax.md", "references/diagram-mermaid.md", "README.md"} {
		if !strings.Contains(result, want) {
			t.Fatalf("expected the listing to mention %q, got: %s", want, result)
		}
	}
	// The bare directory name must never appear as if it were itself a
	// fetchable sibling - it isn't a file and read_skill(file="references")
	// would just error out with "is a directory".
	if strings.Contains(result, "): references,") || strings.Contains(result, "): references\n") ||
		strings.HasSuffix(strings.TrimSpace(result), "references)") {
		t.Fatalf("expected the bare 'references' directory name not to be listed as a fetchable file, got: %s", result)
	}
}

// TestToolReadSkillNestedFileFetchableViaListedPath confirms a path
// obtained from the default call's companion-file listing can be fed
// straight back into "file" and actually resolves to that nested file's
// content - i.e. the listing and the fetch mechanism agree with each
// other end-to-end, not just independently.
func TestToolReadSkillNestedFileFetchableViaListedPath(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "slidev")
	if err := os.MkdirAll(filepath.Join(skillDir, "references"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"),
		[]byte("---\nname: slidev\ndescription: d.\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "references", "diagram-mermaid.md"),
		[]byte("mermaid diagram instructions"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	listing, err := toolReadSkill(map[string]interface{}{"skill": "slidev"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error listing: %v", err)
	}
	if !strings.Contains(listing, "references/diagram-mermaid.md") {
		t.Fatalf("expected listing to include the nested path, got: %s", listing)
	}

	content, err := toolReadSkill(map[string]interface{}{"skill": "slidev", "file": "references/diagram-mermaid.md"}, cfg.Skills)
	if err != nil {
		t.Fatalf("expected the exact listed path to be readable, got error: %v", err)
	}
	if content != "mermaid diagram instructions" {
		t.Fatalf("expected the nested file's real content, got: %q", content)
	}
}

// TestListSkillFilesOnlyListsFilesNotDirectories confirms listSkillFiles
// itself never reports a directory as an entry (only leaf files), across
// multiple nesting levels and multiple sibling subfolders - the shape seen
// in real skills that combine e.g. both "references/" and "assets/"
// folders alongside a top-level README.
func TestListSkillFilesOnlyListsFilesNotDirectories(t *testing.T) {
	root := t.TempDir()
	mustWrite := func(rel, content string) {
		full := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("SKILL.md", "skill")
	mustWrite("README.md", "readme")
	mustWrite("references/core-syntax.md", "a")
	mustWrite("references/diagram-mermaid.md", "b")
	mustWrite("assets/deep/nested/template.txt", "c")

	got := listSkillFiles(root)
	want := []string{
		"README.md",
		"assets/deep/nested/template.txt",
		"references/core-syntax.md",
		"references/diagram-mermaid.md",
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d files, got %d: %v", len(want), len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %q want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	for _, g := range got {
		if g == "SKILL.md" {
			t.Fatal("expected SKILL.md itself to be excluded from the companion-file listing")
		}
	}
}

// TestToolReadSkillCaseInsensitiveLookup confirms a model reproducing the
// skill name with different casing than the system prompt still resolves
// correctly, since local models don't always echo identifiers verbatim.
func TestToolReadSkillCaseInsensitiveLookup(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: Slides.\n---\nBody.\n")
	cfg := loadSkills([]string{root})

	if _, err := toolReadSkill(map[string]interface{}{"skill": "PPTX"}, cfg.Skills); err != nil {
		t.Fatalf("expected case-insensitive lookup to succeed, got: %v", err)
	}
}

// TestToolReadSkillCompanionFileReadable confirms the optional "file"
// argument reads a companion resource relative to that specific skill's
// own folder.
func TestToolReadSkillCompanionFileReadable(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "pptx")
	if err := os.MkdirAll(filepath.Join(skillDir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: pptx\ndescription: d.\n---\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "assets", "notes.md"), []byte("companion notes"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})
	result, err := toolReadSkill(map[string]interface{}{"skill": "pptx", "file": "assets/notes.md"}, cfg.Skills)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "companion notes" {
		t.Fatalf("expected companion file content, got: %q", result)
	}
}

// TestToolReadSkillCompanionFileSandboxed is the security-critical
// regression test: the optional "file" argument must not be usable to
// escape that one skill's own directory via ".." or an absolute path,
// mirroring sandboxedPath's existing guarantee for read_file but rooted at
// the skill's folder instead of the current working directory.
func TestToolReadSkillCompanionFileSandboxed(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: d.\n---\n")
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("should not be readable via read_skill"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := loadSkills([]string{root})

	if _, err := toolReadSkill(map[string]interface{}{"skill": "pptx", "file": "../secret.txt"}, cfg.Skills); err == nil {
		t.Fatal("expected a relative path-traversal escape (..) to be rejected")
	}
	if _, err := toolReadSkill(map[string]interface{}{"skill": "pptx", "file": secret}, cfg.Skills); err == nil {
		t.Fatal("expected an absolute path escaping the skill folder to be rejected")
	}
}

// TestToolReadSkillUnknownNameListsAvailable confirms an unrecognized
// skill name fails with a helpful error that lists what IS available,
// instead of a bare "not found" the model can't act on.
func TestToolReadSkillUnknownNameListsAvailable(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: d.\n---\n")
	cfg := loadSkills([]string{root})

	_, err := toolReadSkill(map[string]interface{}{"skill": "does-not-exist"}, cfg.Skills)
	if err == nil {
		t.Fatal("expected an error for an unknown skill name")
	}
	if !strings.Contains(err.Error(), "pptx") {
		t.Fatalf("expected the error to list available skill names, got: %v", err)
	}
}

// TestToolReadSkillRequiresSkillArg confirms a missing "skill" argument is
// rejected up front with a clear error rather than panicking on a nil
// lookup.
func TestToolReadSkillRequiresSkillArg(t *testing.T) {
	if _, err := toolReadSkill(map[string]interface{}{}, nil); err == nil {
		t.Fatal("expected an error when \"skill\" is not provided")
	}
}

// ─────────────────────────────────────────────────────────────────
// dispatchToolCall integration (same pattern as
// TestDispatchToolCallGetCurrentTime in time_test.go): confirms read_skill
// routes correctly through the real "extra" tool dispatch mechanism ask/
// coding both use, not just the underlying function in isolation.
// ─────────────────────────────────────────────────────────────────

func TestDispatchToolCallReadSkillViaExtra(t *testing.T) {
	root := t.TempDir()
	mustMkdirSkill(t, root, "pptx", "---\nname: pptx\ndescription: Slides.\n---\nFull body text.\n")
	skillsCfg := loadSkills([]string{root})

	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	extra := func(name string, args map[string]interface{}) (string, error, bool) {
		if name != "read_skill" {
			return "", nil, false
		}
		if !skillsCfg.enabled() {
			return "", nil, false
		}
		r, e := toolReadSkill(args, skillsCfg.Skills)
		return r, e, true
	}

	argsJSON, _ := json.Marshal(map[string]interface{}{"skill": "pptx"})
	tc := toolCall{Function: toolCallFunction{Name: "read_skill", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, extra)
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected read_skill to succeed via dispatchToolCall, got: %s", result)
	}
	if !strings.Contains(result, "Full body text.") {
		t.Fatalf("expected the full SKILL.md body in the dispatched result, got: %s", result)
	}

	logged, err := os.ReadFile(outFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "[tool_load_time] read_skill") {
		t.Fatalf("expected a [tool_load_time] entry for read_skill (it's a local-file load), got:\n%s", logged)
	}
}

// TestDispatchToolCallReadSkillUnavailableWhenDisabled confirms that if
// read_skill is somehow called without skills actually being configured
// (e.g. an "extra" wired the same way ask/coding do, but skillsCfg is
// empty), it falls through to "unknown tool" instead of a confusing
// success/failure from an empty skill list - matching how run_command/
// web_search behave when their own feature isn't enabled.
func TestDispatchToolCallReadSkillUnavailableWhenDisabled(t *testing.T) {
	var skillsCfg skillsConfig // zero value: disabled

	dir := t.TempDir()
	outFile, err := os.CreateTemp(dir, "log")
	if err != nil {
		t.Fatal(err)
	}
	defer outFile.Close()

	extra := func(name string, args map[string]interface{}) (string, error, bool) {
		if name != "read_skill" || !skillsCfg.enabled() {
			return "", nil, false
		}
		r, e := toolReadSkill(args, skillsCfg.Skills)
		return r, e, true
	}

	argsJSON, _ := json.Marshal(map[string]interface{}{"skill": "pptx"})
	tc := toolCall{Function: toolCallFunction{Name: "read_skill", Arguments: argsJSON}}

	result := dispatchToolCall(tc, "", "", "", outFile, extra)
	if !strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("expected an ERROR result when skills aren't configured, got: %s", result)
	}
}

// ─────────────────────────────────────────────────────────────────
// End-to-end: cmdAsk wired with --skills-dir, driven through a scripted
// mock model (same shape as TestCmdAskAutoVerifyLoop in
// ask_integration_test.go), confirming the full path from flag parsing
// through system-prompt injection, tool advertisement, and dispatch.
// ─────────────────────────────────────────────────────────────────

func TestCmdAskReadSkillEndToEnd(t *testing.T) {
	workDir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	skillsDir := t.TempDir()
	mustMkdirSkill(t, skillsDir, "thai-writing",
		"---\nname: thai-writing\ndescription: แนวทางการเขียนบทความภาษาไทยแบบกระชับ\n---\n"+
			"# Thai writing\nเขียนให้กระชับและเป็นธรรมชาติ ไม่ใช้คำฟุ่มเฟือย\n")

	var round int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&round, 1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		body, _ := io.ReadAll(r.Body)
		if n == 1 {
			// The AVAILABLE SKILLS section must have actually reached the
			// model in the system prompt for it to know to call read_skill.
			if !strings.Contains(string(body), "thai-writing") || !strings.Contains(string(body), "AVAILABLE SKILLS") {
				t.Errorf("expected the request payload to include the AVAILABLE SKILLS section mentioning thai-writing, got: %s", body)
			}
			fmt.Fprint(w, streamLine("", "read_skill", `{"skill":"thai-writing"}`, true))
			return
		}
		fmt.Fprint(w, streamLine("เขียนตามแนวทาง skill แล้วครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-skill.log", "--skills-dir", skillsDir, "เขียนบทความสั้นๆ เกี่ยวกับกาแฟ"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if got := atomic.LoadInt32(&round); got != 2 {
		t.Fatalf("expected exactly 2 rounds (read_skill, final answer), got %d", got)
	}

	log, err := os.ReadFile("ask-skill.log")
	if err != nil {
		t.Fatalf("expected output log to exist: %v", err)
	}
	if !strings.Contains(string(log), "read_skill") {
		t.Fatalf("expected the tool_call log to record read_skill, got:\n%s", log)
	}
	if !strings.Contains(string(log), "เขียนให้กระชับและเป็นธรรมชาติ") {
		t.Fatalf("expected the read_skill tool result (full SKILL.md body) to be logged, got:\n%s", log)
	}
	if !strings.Contains(string(log), "# skills: enabled") {
		t.Fatalf("expected the log header to report skills enabled, got:\n%s", log)
	}
}

// TestCmdAskWithoutSkillsDirNeverOffersReadSkill confirms a completely
// ordinary session (no --skills-dir/OLA_SKILLS_DIR at all) never even
// advertises read_skill - skills must stay entirely invisible/inert unless
// explicitly configured, same principle as run_command/web_search.
func TestCmdAskWithoutSkillsDirNeverOffersReadSkill(t *testing.T) {
	workDir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(workDir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/chat", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprint(w, streamLine("รับทราบครับ", "", "", true))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	os.Setenv("OLA_OLLAMA_API_BASE", srv.URL)
	defer os.Unsetenv("OLA_OLLAMA_API_BASE")
	os.Unsetenv("OLA_SKILLS_DIR")

	exitCode := cmdAsk([]string{"-m", "mock-model", "-o", "ask-noskill.log", "สวัสดีครับ"})
	if exitCode != 0 {
		t.Fatalf("expected cmdAsk to exit 0, got %d", exitCode)
	}
	if strings.Contains(gotBody, "read_skill") {
		t.Fatalf("expected read_skill to never appear in the request when no skills directory is configured, got: %s", gotBody)
	}
	if strings.Contains(gotBody, "AVAILABLE SKILLS") {
		t.Fatalf("expected no AVAILABLE SKILLS section in the system prompt when skills are disabled, got: %s", gotBody)
	}
}
