// ola - unified CLI combining the features of ola-ask.fish and ola-extract.fish
//
// Subcommands:
//   ola ask [options] <prompt> [files...]   - call Ollama /api/chat with streaming
//   ola extract <input_file>                - extract <<<ooo FILE ooo>>> blocks into files
//
// Environment variables (renamed from the original fish scripts):
//   OLA_OLLAMA_API_BASE     Host (default: http://localhost:11434)
//   OLA_OLLAMA_API_KEY      Bearer token (enabled with -k)
//   OLA_OLLAMA_MODEL        Model to use (override with -m) [required unless -m is set]
//   OLA_OLLAMA_CONTEXT_SIZE Default num_ctx (override with -c, default: 16384)
//   OLA_OUTPUT_FILE         Default output file (override with -o, default: output.txt)
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// System prompt (default, built-in). Backticks are represented with
// the placeholder @@BT@@ because Go raw strings cannot contain a
// literal backtick; it is substituted back at init time.
// ─────────────────────────────────────────────────────────────────

const rawSystemPrompt = `# ROLE
    You are a Senior IT Architect and Senior Programmer. You operate with extreme precision, discipline, and a "production-ready" mindset. Output must be defect-free, performant, and secure.

    # CORE OPERATING PRINCIPLES
    1. Correctness over speed.
    2. Evidence over assumption. Never fabricate APIs or syntax.
    3. Logical convergence. Self-review is a rigorous loop, not a single pass.
    4. Minimalist communication. Deliver results, not filler.
    5. **[CRITICAL] Latest known-good versions only.** Always use the highest version of every tool, library, and framework that you have verified knowledge of and can use without error. Using outdated versions when a newer correct version is known is a defect.

    # MANDATORY WORKFLOW

    ## Stage 1 — Requirement Analysis & Contextual Defaults
    - Identify requirements and constraints.
    - **Produce a numbered Requirements Checklist** from all stated and implied requirements. This list is the ground truth used in Stage 5.
    - **Ambiguity Handling:** If an ambiguity is critical, ASK. If it is minor or standard, apply **Industry Best Practices** as a "Reasonable Default" and list it as an assumption in the final delivery.

    ## Stage 2 — Strategic Planning
    - Break the task into concrete subtasks.
    - Identify all edge cases (empty states, concurrency, scale, security, etc.) before writing a single line of code.

    ## Stage 3 — Execution
    - Implement the solution strictly following the plan and handling all identified edge cases.
    - Use only verified APIs and syntax. No placeholders (e.g., "TODO", "...").

    ## Stage 4 — Critical Self-Review Loop (Max 3 Iterations)
    Execute this loop to reach convergence. To avoid the "Self-Correction Blind Spot," you must actively act as a **Hostile Reviewer** trying to break your own logic.

    1. **Verification Pass:** Check against:
       - Full requirement coverage & Edge case handling.
       - Logic integrity & Syntax correctness (mental trace).
       - Security (Injection, Secrets, Memory safety).
       - Performance (Complexity analysis).
    2. **Refine:** If any defect or inconsistency is found, fix the root cause and propagate the fix everywhere.
    3. **Loop:** Repeat until zero defects remain OR you reach a **maximum of 3 iterations**. If 3 passes are reached without full convergence, deliver the most stable version and explicitly flag the remaining risks.

    ## Stage 5 — Requirements Coverage Gate ⚠️ MANDATORY — NO BYPASS

    This stage runs **after Stage 4** and **before delivery**. Its sole purpose is to guarantee that every requirement identified in Stage 1 has a traceable implementation in the produced codebase.

    ### 5.1 — Build Coverage Matrix

    Reconstruct the numbered Requirements Checklist from Stage 1. For each requirement, map it to the specific file, function, class, or configuration block that implements it:

    | # | Requirement | Implemented In | Status |
    |---|-------------|---------------|--------|
    | 1 | <req text>  | @@BT@@src/foo.rs:fn bar()@@BT@@ | ✅ COVERED |
    | 2 | <req text>  | — | ❌ MISSING |

    ### 5.2 — Gap Resolution Loop (Max 3 Iterations)

    If **any** row has Status = ❌ MISSING:

    1. **Return to Stage 3.** Implement all missing requirements. Do not skip or partially implement — each missing requirement must be fully resolved.
    2. **Re-run Stage 4** on the updated codebase.
    3. **Re-run Stage 5.1** to rebuild the coverage matrix.
    4. Repeat until the matrix contains **zero ❌ rows**, or until **3 iterations** are exhausted.

    If 3 iterations are exhausted and gaps remain, halt and explicitly report:
    - Which requirements are still unimplemented.
    - The root cause (ambiguity, technical blocker, scope conflict).
    - Do NOT proceed to delivery with silent gaps.

    ### 5.3 — Coverage Confirmation

    When all rows are ✅ COVERED, emit a single line before the delivery section:

    @@BT@@@@BT@@@@BT@@
    [COVERAGE GATE PASSED] All N requirements verified with traceable implementations.
    @@BT@@@@BT@@@@BT@@

    Only after this confirmation may execution advance to Stage 6.

    ## Stage 6 — Minimalist Delivery
    - **Primary Output:** Deliver the actual code, architecture, or answer immediately.
    - **Essential Commentary Only:** Include ONLY critical explanations (Safety, Security, or Core Logic). Omit general descriptions, "how-to" guides, or introductory filler.
    - **Metadata:** If industry defaults were used or risks remain, list them briefly as "Assumptions" or "Risks".

    # DEPENDENCY VERSION POLICY ⚠️ HIGH PRIORITY

    ## The Rule (non-negotiable)
    For every dependency used in code — language runtime, library, framework, package, CLI tool, container image, or API — you MUST use the **highest version you have verified, working knowledge of**.
    Defaulting to an older version when a newer correct version is known is a **critical defect**, equivalent to shipping broken code.

    ## Version Selection Process (mandatory, runs at Stage 1)
    For each dependency identified in the task:
    1. **Recall** the highest version you have reliable knowledge of for that dependency.
    2. **Verify mentally** that you know the correct API, syntax, and breaking changes for that version.
    3. **Use that version.** Do not fall back to an older version out of caution — if you know it, use it.
    4. **If genuinely uncertain** about the latest version, state it explicitly as an Assumption: "Used X vN — latest confirmed version in my knowledge. Verify against current release."

    ## Scope: What This Covers
    - Language runtimes: Python, Node.js, Rust, Go, Java, etc.
    - Package managers & lock files: Cargo.toml, package.json, go.mod, pyproject.toml, etc.
    - Frameworks: React, Axum, FastAPI, Spring Boot, etc.
    - Container base images: @@BT@@ubuntu:24.04@@BT@@ not @@BT@@ubuntu:20.04@@BT@@; @@BT@@rust:1.78@@BT@@ not @@BT@@rust:1.65@@BT@@
    - CLI tools and build systems: webpack, vite, cmake, etc.
    - Database drivers and ORMs
    - Cloud SDK versions

    ## Examples

    CORRECT — uses latest known versions:
    <<<ooo pyproject.toml ooo>>>
    [project]
    name = "myapp"
    requires-python = ">=3.12"
    dependencies = [
        "fastapi>=0.111.0",
        "pydantic>=2.7.0",
        "uvicorn[standard]>=0.29.0",
    ]
    <<<xxx pyproject.toml xxx>>>

    WRONG — uses stale versions without justification (FORBIDDEN):
    <<<ooo pyproject.toml ooo>>>
    [project]
    requires-python = ">=3.8"      ← outdated when 3.12 is known
    dependencies = [
        "fastapi>=0.95.0",         ← outdated when 0.111.0 is known
        "pydantic>=1.10.0",        ← major version behind, breaking API differences
    ]
    <<<xxx pyproject.toml xxx>>>

    ## Version Conflict Rule
    If the user explicitly pins a version (e.g. @@BT@@django==3.2@@BT@@), respect it and do NOT upgrade silently.
    If the pinned version is significantly outdated and poses a security or compatibility risk, flag it:
    "Risk: django 3.2 is EOL. Consider upgrading to 5.x."

    ## Self-Check: Version Gate (runs at Stage 4)
    Before finalizing any file containing dependencies:
    1. List every dependency version used.
    2. For each: confirm it is the highest version in your verified knowledge.
    3. If any version is lower than what you know to be available, upgrade it.
    4. If uncertain, add an explicit Assumption note — never silently use an old version.

    # FILE OUTPUT FORMAT (STRICT)

    ## Tag Anatomy (memorize exactly)
    Opening tag: <<<ooo FILENAME ooo>>>
    Closing tag:  <<<xxx FILENAME xxx>>>

    Rules:
    - FILENAME in both tags MUST be identical (same case, same extension).
    - Tags occupy their OWN LINE — no text before or after on the same line.
    - File content goes between the two tags, verbatim, no truncation.
    - Multiple files: emit them sequentially, one block per file.
    - NEVER use any other delimiter (no @@BT@@@@BT@@@@BT@@, no <file>, no [FILE], no === borders).
    - NEVER omit the closing tag.
    - The marker characters are EXACTLY three lowercase letter 'o' (ooo) for open and EXACTLY three lowercase letter 'x' (xxx) for close. Do NOT use 2 or 4 letters. Do NOT use uppercase.

    ## Correct Examples

    Single file:
    <<<ooo main.py ooo>>>
    def hello():
        print("hello world")

    if __name__ == "__main__":
        hello()
    <<<xxx main.py xxx>>>

    Multiple files:
    <<<ooo src/server.rs ooo>>>
    fn main() {
        println!("server start");
    }
    <<<xxx src/server.rs xxx>>>

    <<<ooo Cargo.toml ooo>>>
    [package]
    name = "server"
    version = "0.1.0"
    edition = "2021"
    <<<xxx Cargo.toml xxx>>>

    File with path:
    <<<ooo config/nginx.conf ooo>>>
    server {
        listen 80;
        server_name example.com;
    }
    <<<xxx config/nginx.conf xxx>>>

    ## Common Mistakes — FORBIDDEN

    WRONG (wrong open marker — 2 letters):
    <<<oo main.py oo>>>
    ...
    <<<xx main.py xx>>>

    WRONG (wrong open marker — 4 letters):
    <<<oooo main.py oooo>>>
    ...
    <<<xxxx main.py xxxx>>>

    WRONG (mismatched filename between open and close):
    <<<ooo main.py ooo>>>
    ...
    <<<xxx app.py xxx>>>

    WRONG (tags not on their own line):
    some text <<<ooo main.py ooo>>> more text

    WRONG (using code fence instead):
    @@BT@@@@BT@@@@BT@@python
    ...
    @@BT@@@@BT@@@@BT@@

    WRONG (missing closing tag):
    <<<ooo main.py ooo>>>
    ...content without closing tag

    ## Self-Check Before Output
    Before emitting any file block, verify:
    1. Open tag starts with exactly: <<<ooo
    2. Close tag starts with exactly: <<<xxx
    3. FILENAME is identical in both tags
    4. Each tag is on its own dedicated line
    5. Closing tag is present

    # CHANGED FILES ONLY — ZERO REDUNDANCY POLICY

    ## The Rule (absolute, no exceptions)
    When the user provides existing file content and requests modifications:
    **ONLY emit files whose content has actually changed.**
    A file that is identical to the input — byte for byte, line for line — MUST NOT be emitted.
    Emitting unchanged files is a defect. Treat it the same as emitting incorrect code.

    ## Full Content Requirement (critical)
    When a file IS emitted, it MUST contain the **complete, final file content** — every line from top to bottom.
    NEVER emit partial content, diffs, snippets, or placeholders such as:
      "// ... rest of file unchanged ..."
      "# same as before"
      "... (omitted for brevity) ..."
    The file block must be directly usable as a drop-in replacement with no manual merging by the user.

    ## Decision Logic Per File
    For each file in the task, before emitting, ask:
      "Does this file have at least ONE line that differs from the input?"
      YES → emit it with FULL content inside the file tags.
      NO  → do NOT emit it. List its name under "Unchanged Files" instead.

    ## How to Report Unchanged Files
    After all changed-file blocks, add a plain-text summary:

    Unchanged (not re-emitted): config.py, tests/test_utils.py, README.md

    Do NOT wrap unchanged filenames in file tags. Just list them.
    If ALL files changed, omit the summary entirely.
    If NO files changed (e.g. the request had no effect), state: "No changes required." and emit nothing.

    ## Examples

    Scenario: user provides 3 files (main.rs, lib.rs, Cargo.toml).
    Task: add a log line inside fn main() in main.rs only.

    CORRECT output (main.rs emitted in full; others skipped):
    <<<ooo src/main.rs ooo>>>
    use std::io;

    fn helper() -> u32 {
        42
    }

    fn main() {
        println!("[LOG] starting");   // ← new line added
        let result = helper();
        println!("result: {}", result);
    }
    <<<xxx src/main.rs xxx>>>

    Unchanged (not re-emitted): src/lib.rs, Cargo.toml

    WRONG — emits unchanged files (FORBIDDEN):
    <<<ooo src/main.rs ooo>>>
    ...full content...
    <<<xxx src/main.rs xxx>>>

    <<<ooo Cargo.toml ooo>>>
    [package]
    name = "server"    ← identical to input, MUST NOT be emitted
    version = "0.1.0"
    <<<xxx Cargo.toml xxx>>>

    WRONG — emits partial content with placeholder (FORBIDDEN):
    <<<ooo src/main.rs ooo>>>
    fn main() {
        println!("[LOG] starting");   // ← new line
        // ... rest of file unchanged ...    ← FORBIDDEN placeholder
    }
    <<<xxx src/main.rs xxx>>>

    ## Why This Is Critical
    - Re-emitting identical files wastes context window.
    - It forces the user to diff against originals to find what actually changed.
    - Partial content forces the user to manually merge — defeats the purpose of the edit.
    - This policy is STRICT: when in doubt, do NOT emit. List it as unchanged instead.

    ## Self-Check: Changed-Files Gate
    Before emitting each file block, run this gate:
    1. Compare the complete file you are about to emit against the input version line by line.
    2. If zero differences exist → STOP. Do not emit. Add to the unchanged list.
    3. If at least one difference exists → emit the FULL file content using the file tags.
    4. Confirm the file block contains no placeholder or omission markers.
    This gate runs AFTER the file-tag self-check above. Both must pass.

    # COMMUNICATION RULES
    - **No Filler:** No "Certainly", "Here is the solution", or "I hope this helps".
    - **No LaTeX in Prose:** Use standard characters for regular text.
    - **Direct & Technical:** Speak peer-to-peer (Senior to Senior).
    - **Deliverable-First:** If the user asks for code, the response should ideally start with the first file tag or the direct answer.`

var defaultSystemPrompt = strings.ReplaceAll(rawSystemPrompt, "@@BT@@", "`")

// ─────────────────────────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) < 2 {
		printTopUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "ask":
		os.Exit(cmdAsk(os.Args[2:]))
	case "extract":
		os.Exit(cmdExtract(os.Args[2:]))
	case "-h", "--help", "help":
		printTopUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown subcommand '%s'\n\n", os.Args[1])
		printTopUsage()
		os.Exit(1)
	}
}

func printTopUsage() {
	fmt.Println("Usage: ola <command> [options]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  ask      Call Ollama /api/chat with prompt, files, ctx, system prompt, auth, thinking, timing, and output logging")
	fmt.Println("  extract  Extract <<<ooo FILE ooo>>> ... <<<xxx FILE xxx>>> blocks from a file and write them to disk")
	fmt.Println()
	fmt.Println("Run 'ola ask -h' or 'ola extract -h' for command-specific help.")
}

// ─────────────────────────────────────────────────────────────────
// ask subcommand
// ─────────────────────────────────────────────────────────────────

type ollamaMessage struct {
	Role     string   `json:"role"`
	Content  string   `json:"content"`
	Thinking string   `json:"thinking,omitempty"`
	Images   []string `json:"images,omitempty"`
}

type ollamaOptions struct {
	NumCtx int `json:"num_ctx"`
}

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Options  ollamaOptions   `json:"options"`
	Stream   bool            `json:"stream"`
	Think    *bool           `json:"think,omitempty"`
}

type ollamaStreamChunk struct {
	Message struct {
		Role     string `json:"role"`
		Content  string `json:"content"`
		Thinking string `json:"thinking"`
	} `json:"message"`
	Done              bool   `json:"done"`
	Error             string `json:"error"`
	PromptEvalCount   int    `json:"prompt_eval_count"`
	EvalCount         int    `json:"eval_count"`
	EvalDuration      int64  `json:"eval_duration"`
	PromptEvalDurLast int64  `json:"prompt_eval_duration"`
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
}

func askUsage(fs *flag.FlagSet) func() {
	return func() {
		fmt.Println("Usage: ola ask [options] <prompt> [files...]")
		fmt.Println()
		fmt.Println("เรียก Ollama ผ่าน HTTP API (/api/chat) พร้อม streaming + thinking + timing")
		fmt.Println()
		fmt.Println("Environment variables:")
		fmt.Println("  OLA_OLLAMA_API_BASE       Host (default: http://localhost:11434)")
		fmt.Println("  OLA_OLLAMA_API_KEY        Bearer token (เปิดใช้ด้วย -k)")
		fmt.Println("  OLA_OLLAMA_MODEL          โมเดลที่จะใช้ (override ด้วย -m) [จำเป็น ถ้าไม่ใช้ -m]")
		fmt.Println("  OLA_OLLAMA_CONTEXT_SIZE   num_ctx เริ่มต้น (override ด้วย -c, default: 16384)")
	fmt.Println("  OLA_OUTPUT_FILE           ไฟล์ output เริ่มต้น (override ด้วย -o, default: output.txt)")
	fmt.Println("  OLA_TOPIC                 topic สำหรับส่ง notification ไป ntfy.sh (override ด้วย -x)")
	fmt.Println()
	fmt.Println("Options: (ต้องระบุก่อน <prompt> เสมอ ทั้งหมดรองรับทั้งรูปแบบสั้น -x และยาว --xxx)")
		fmt.Println("  -m, --model <name>   โมเดลที่ใช้ [จำเป็น ถ้าไม่ตั้ง $OLA_OLLAMA_MODEL]")
		fmt.Println("  -c, --ctx <num>      ตั้ง num_ctx ต่อ request ต้องเป็นจำนวนเต็มไม่ติดลบ (default: $OLA_OLLAMA_CONTEXT_SIZE หรือ 16384)")
		fmt.Println("  -k, --key            ส่ง Authorization: Bearer $OLA_OLLAMA_API_KEY (error ถ้าตั้ง -k แต่ไม่มีค่าตัวแปรนี้)")
		fmt.Println("  -s, --system <file>  ใช้ system prompt จากไฟล์ระบุ แทนค่า built-in (error ถ้าไฟล์ไม่มี อ่านไม่ได้ หรือว่างเปล่า)")
		fmt.Println("  -T, --no-think       ปิด thinking mode โดยส่ง \"think\": false ไปใน request (default: ไม่ส่ง field นี้ ให้ Ollama ตัดสินใจเอง)")
	fmt.Println("  -x, --topic <topic>  ส่ง notification ไป ntfy.sh ด้วย topic นี้เมื่อทำงานเสร็จ (override $OLA_TOPIC)")
	fmt.Println("  -o, --output <file>  บันทึกผลลัพธ์ + log ลงไฟล์ (default: $OLA_OUTPUT_FILE หรือ output.txt) เขียนทับไฟล์เดิมเสมอ เว้นแต่ใช้ -a")
	fmt.Println("  -a, --append         ต่อท้ายไฟล์ output แทนการเขียนทับ (ใช้ได้ทั้งกับ -o หรือไฟล์ default ก็ได้ ไม่จำเป็นต้องคู่กับ -o)")
		fmt.Println("  -r, --raw            ไม่ใส่ separator \"===== แนบไฟล์ =====\" และ \"--- filename ---\" ระหว่างไฟล์ข้อความที่แนบ")
		fmt.Println("  -n, --dry-run        แสดง JSON payload และ system prompt ที่จะส่ง โดยไม่เรียก API จริง")
		fmt.Println("  -h, --help           แสดงข้อความนี้")
		fmt.Println()
		fmt.Println("ไฟล์แนบ ([files...]):")
		fmt.Println("  - ไฟล์นามสกุล .jpg .jpeg .png .webp .gif จะถูกอ่านและแนบเป็น base64 ใน field \"images\" ของ user message")
		fmt.Println("  - ไฟล์นามสกุลอื่นทั้งหมดจะถูกอ่านเป็นข้อความและต่อท้ายเข้าไปใน content ของ prompt โดยตรง")
		fmt.Println("  - ไฟล์ที่ไม่พบจะแสดง warning และถูกข้ามไป ไม่ทำให้โปรแกรมหยุดทำงาน")
		fmt.Println()
	fmt.Println("หมายเหตุ:")
	fmt.Println("  - ไม่ต้องพึ่งพา curl/jq/perl/base64 ภายนอกอีกต่อไป ทำงานแบบ native ทั้งหมดใน Go binary เดียว")
	fmt.Println("  - Exit code จะเป็น 1 ถ้า Ollama ตอบกลับด้วย HTTP status >= 400 (เนื้อหาที่ตอบกลับมาจะยังถูกแสดง/บันทึกตามปกติ)")
	fmt.Println("  - ใช้ -x <topic> หรือตั้งตัวแปร OLA_TOPIC เพื่อรับ notification ผ่าน ntfy.sh เมื่อทำงานเสร็จ")
	fmt.Println("    (notification จะถูกส่งทั้งในกรณีสำเร็จและเกิด error ไปที่ https://ntfy.sh/<topic>)")
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  export OLA_OLLAMA_MODEL=qwen3.6:27b")
		fmt.Println("  ola ask 'review this code' main.py")
		fmt.Println("  ola ask -k -c 65536 'วิเคราะห์' src/*.py")
		fmt.Println("  ola ask -s system.md 'แปล' input.txt")
	fmt.Println("  ola ask -x mytopic 'review this PR'")
	fmt.Println("  export OLA_TOPIC=mytopic")
	fmt.Println("  ola ask 'deploy to production'  # ใช้ค่า OLA_TOPIC จาก environment")
	}
}

func cmdAsk(args []string) int {
	fs := flag.NewFlagSet("ask", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // we print our own errors

	var model, ctxStr, systemFile, outputFile, topic string
	var flagKey, flagNoThink, flagRaw, flagDryRun, flagAppend, flagHelp bool

	fs.StringVar(&model, "m", "", "")
	fs.StringVar(&model, "model", "", "")
	fs.StringVar(&ctxStr, "c", "", "")
	fs.StringVar(&ctxStr, "ctx", "", "")
	fs.BoolVar(&flagKey, "k", false, "")
	fs.BoolVar(&flagKey, "key", false, "")
	fs.StringVar(&systemFile, "s", "", "")
	fs.StringVar(&systemFile, "system", "", "")
	fs.BoolVar(&flagNoThink, "T", false, "")
	fs.BoolVar(&flagNoThink, "no-think", false, "")
	fs.BoolVar(&flagRaw, "r", false, "")
	fs.BoolVar(&flagRaw, "raw", false, "")
	fs.BoolVar(&flagDryRun, "n", false, "")
	fs.BoolVar(&flagDryRun, "dry-run", false, "")
	fs.StringVar(&outputFile, "o", "", "")
	fs.StringVar(&outputFile, "output", "", "")
	fs.BoolVar(&flagAppend, "a", false, "")
	fs.BoolVar(&flagAppend, "append", false, "")
	fs.StringVar(&topic, "x", "", "")
	fs.StringVar(&topic, "topic", "", "")
	fs.BoolVar(&flagHelp, "h", false, "")
	fs.BoolVar(&flagHelp, "help", false, "")

	usage := askUsage(fs)
	fs.Usage = usage

	if err := fs.Parse(args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	if flagHelp {
		usage()
		return 0
	}

	rest := fs.Args()
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "error: ต้องระบุ prompt อย่างน้อย")
		return 1
	}

	// Host + Auth
	host := os.Getenv("OLA_OLLAMA_API_BASE")
	if host == "" {
		host = "http://localhost:11434"
	}
	host = strings.TrimRight(host, "/")

	var apiKey string
	if flagKey {
		apiKey = os.Getenv("OLA_OLLAMA_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "error: -k ระบุไว้ แต่ OLA_OLLAMA_API_KEY ไม่ได้ตั้งหรือว่างเปล่า")
			return 1
		}
	}

	// System prompt
	systemPrompt := defaultSystemPrompt
	systemSource := "built-in"
	if systemFile != "" {
		info, err := os.Stat(systemFile)
		if err != nil || info.IsDir() {
			fmt.Fprintf(os.Stderr, "error: ไม่พบไฟล์ system prompt: %s\n", systemFile)
			return 1
		}
		data, err := os.ReadFile(systemFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: อ่านไฟล์ system prompt ไม่ได้: %s\n", systemFile)
			return 1
		}
		content := strings.TrimRight(string(data), "\n")
		if strings.TrimSpace(content) == "" {
			fmt.Fprintf(os.Stderr, "error: ไฟล์ system prompt ว่างเปล่า: %s\n", systemFile)
			return 1
		}
		systemPrompt = content
		systemSource = "file: " + systemFile
	}

	// Model
	if model == "" {
		model = os.Getenv("OLA_OLLAMA_MODEL")
	}
	if model == "" {
		fmt.Fprintln(os.Stderr, "error: ต้องระบุโมเดลผ่าน -m/--model หรือตั้งค่าตัวแปร OLA_OLLAMA_MODEL")
		return 1
	}

	// Context size
	if ctxStr == "" {
		ctxStr = os.Getenv("OLA_OLLAMA_CONTEXT_SIZE")
	}
	if ctxStr == "" {
		ctxStr = "16384"
	}
	if !regexp.MustCompile(`^[0-9]+$`).MatchString(ctxStr) {
		fmt.Fprintf(os.Stderr, "error: ctx ต้องเป็นตัวเลข (got: %s)\n", ctxStr)
		return 1
	}
	ctx, _ := strconv.Atoi(ctxStr)

	// Output file
	if outputFile == "" {
		outputFile = os.Getenv("OLA_OUTPUT_FILE")
	}
	if outputFile == "" {
		outputFile = "output.txt"
	}

	prompt := rest[0]
	files := rest[1:]

	// Separate image / text files
	var imageFiles, textFiles []string
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			fmt.Fprintf(os.Stderr, "warning: ไม่พบไฟล์ %s\n", f)
			continue
		}
		ext := strings.ToLower(filepath.Ext(f))
		if imageExts[ext] {
			imageFiles = append(imageFiles, f)
		} else {
			textFiles = append(textFiles, f)
		}
	}

	content := prompt
	if len(textFiles) > 0 {
		if !flagRaw {
			content += "\n\n===== แนบไฟล์ ====="
		}
		for _, f := range textFiles {
			if !flagRaw {
				content += fmt.Sprintf("\n\n--- %s ---", f)
			}
			data, err := os.ReadFile(f)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: อ่านไฟล์ %s ไม่ได้\n", f)
				continue
			}
			content += "\n" + string(data)
		}
	}

	userMsg := ollamaMessage{Role: "user", Content: content}
	for _, img := range imageFiles {
		data, err := os.ReadFile(img)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: encode รูป %s ไม่ได้\n", img)
			return 1
		}
		userMsg.Images = append(userMsg.Images, base64.StdEncoding.EncodeToString(data))
	}

	var messages []ollamaMessage
	if systemPrompt != "" {
		messages = append(messages, ollamaMessage{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, userMsg)

	req := ollamaRequest{
		Model:    model,
		Messages: messages,
		Options:  ollamaOptions{NumCtx: ctx},
		Stream:   true,
	}
	if flagNoThink {
		f := false
		req.Think = &f
	}

	payload, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: สร้าง JSON payload ไม่ได้: %v\n", err)
		return 1
	}

	// Dry-run
	if flagDryRun {
		fmt.Printf("── POST %s/api/chat ──\n", host)
		if flagKey {
			fmt.Printf("── Header: Authorization: Bearer %s ──\n", maskKey(apiKey))
		}
		fmt.Printf("── System prompt (%s) ──\n", systemSource)
		fmt.Println(systemPrompt)
		fmt.Println("── End system prompt ──")
		fmt.Printf("── Output file: %s ──\n", outputFile)
		var pretty map[string]interface{}
		_ = json.Unmarshal(payload, &pretty)
		prettyBytes, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(prettyBytes))
		return 0
	}

	// Prepare output file
	var outFile *os.File
	if flagAppend {
		outFile, err = os.OpenFile(outputFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	} else {
		outFile, err = os.OpenFile(outputFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: เขียนไฟล์ %s ไม่ได้\n", outputFile)
		return 1
	}
	defer outFile.Close()

	fmt.Fprintf(outFile, "# ola-ask %s\n", time.Now().Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(outFile, "# host: %s\n", host)
	fmt.Fprintf(outFile, "# model: %s\n", model)
	fmt.Fprintf(outFile, "# num_ctx: %d\n", ctx)
	if flagNoThink {
		fmt.Fprintln(outFile, "# thinking: disabled")
	} else {
		fmt.Fprintln(outFile, "# thinking: enabled (default)")
	}
	if flagKey {
		fmt.Fprintln(outFile, "# auth: Bearer (OLA_OLLAMA_API_KEY)")
	}
	fmt.Fprintln(outFile, "# prompt:")
	for _, line := range strings.Split(prompt, "\n") {
		fmt.Fprintf(outFile, "#   %s\n", line)
	}
	if len(files) > 0 {
		fmt.Fprintf(outFile, "# files: %s\n", strings.Join(files, ", "))
	}
	fmt.Fprintln(outFile, "---")
	fmt.Fprintln(outFile)

	// Resolve ntfy.sh topic early so it's available on all exit paths
	ntfyTopic := topic
	if ntfyTopic == "" {
		ntfyTopic = os.Getenv("OLA_TOPIC")
	}

	// Send request
	httpReq, err := http.NewRequest(http.MethodPost, host+"/api/chat", strings.NewReader(string(payload)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: สร้าง HTTP request ไม่ได้: %v\n", err)
		if ntfyTopic != "" {
			sendNotification(ntfyTopic, fmt.Sprintf("Work Failed: %s", err.Error()))
		}
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if flagKey {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{}
	resp, err := client.Do(httpReq)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: เรียก API ไม่สำเร็จ: %v\n", err)
		if ntfyTopic != "" {
			sendNotification(ntfyTopic, fmt.Sprintf("Work Failed: %s", err.Error()))
		}
		return 1
	}
	defer resp.Body.Close()

	streamResponse(resp.Body, outFile)

	// Send ntfy.sh notification based on response status
	if ntfyTopic != "" {
		if resp.StatusCode >= 400 {
			sendNotification(ntfyTopic, fmt.Sprintf("Work Failed: %s", resp.Status))
		} else {
			sendNotification(ntfyTopic, "Work Finnished")
		}
	}

	if resp.StatusCode >= 400 {
		return 1
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────
// ntfy.sh notification
// ─────────────────────────────────────────────────────────────────

func sendNotification(topic, message string) {
	url := "https://ntfy.sh/" + topic
	resp, err := http.Post(url, "text/plain", strings.NewReader(message))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: ส่ง notification ไม่สำเร็จ: %v\n", err)
		return
	}
	resp.Body.Close()
}

func maskKey(key string) string {
	r := []rune(key)
	if len(r) <= 10 {
		return key
	}
	return string(r[:6]) + "…" + string(r[len(r)-4:])
}

func isTerminalStdout() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}

func fmtDur(d time.Duration) string {
	s := d.Seconds()
	if s < 60 {
		return fmt.Sprintf("%.1fs", s)
	} else if s < 3600 {
		m := int(s / 60)
		rem := s - float64(m)*60
		return fmt.Sprintf("%dm %.1fs", m, rem)
	}
	h := int(s / 3600)
	m := int((s - float64(h)*3600) / 60)
	rem := s - float64(h)*3600 - float64(m)*60
	return fmt.Sprintf("%dh %dm %.1fs", h, m, rem)
}

func streamResponse(body io.Reader, outFile *os.File) {
	isTTY := isTerminalStdout()
	var cyan, bold, dim, reset string
	if isTTY {
		cyan, bold, dim, reset = "\x1b[96m", "\x1b[1m", "\x1b[2m", "\x1b[0m"
	}

	state := ""
	start := time.Now()
	var thinkStart time.Time
	var thinkDuration time.Duration
	var promptTokens, evalTokens int
	var evalDurationNS int64

	reader := bufio.NewReaderSize(body, 1<<20)
	for {
		line, err := reader.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed != "" {
			var chunk ollamaStreamChunk
			if jsonErr := json.Unmarshal([]byte(trimmed), &chunk); jsonErr == nil {
				if chunk.Error != "" {
					msg := "\nERROR: " + chunk.Error + "\n"
					fmt.Print(msg)
					fmt.Fprint(outFile, msg)
				} else {
					think := chunk.Message.Thinking
					content := chunk.Message.Content
					if think != "" {
						if state != "T" {
							thinkStart = time.Now()
							fmt.Print(cyan + " <<<--Thinking-->>>\n")
							fmt.Fprint(outFile, "<<<--Thinking-->>>\n")
							state = "T"
						}
						fmt.Print(think)
						fmt.Fprint(outFile, think)
					}
					if content != "" {
						if state == "T" {
							thinkDuration = time.Since(thinkStart)
							fmt.Print(reset + "\n\n" + bold + " <<<--Answer-->>>" + reset + "\n")
							fmt.Fprint(outFile, "\n\n<<<--Answer-->>>\n")
						}
						state = "A"
						fmt.Print(content)
						fmt.Fprint(outFile, content)
					}
					if chunk.Done {
						promptTokens = chunk.PromptEvalCount
						evalTokens = chunk.EvalCount
						evalDurationNS = chunk.EvalDuration
					}
				}
			}
		}
		if err != nil {
			break
		}
	}

	if state == "T" && thinkDuration == 0 {
		thinkDuration = time.Since(thinkStart)
		fmt.Print(reset)
	}
	total := time.Since(start)
	totalStr := fmtDur(total)
	if thinkDuration > 0 {
		thinkStr := fmtDur(thinkDuration)
		fmt.Printf("\n\n%s⏱  thinking: %s  |  total: %s%s\n", dim, thinkStr, totalStr, reset)
		fmt.Fprintf(outFile, "\n\n⏱  thinking: %s  |  total: %s\n", thinkStr, totalStr)
	} else {
		fmt.Printf("\n\n%s⏱  total: %s%s\n", dim, totalStr, reset)
		fmt.Fprintf(outFile, "\n\n⏱  total: %s\n", totalStr)
	}

	totalTokens := promptTokens + evalTokens
	if totalTokens > 0 {
		var tps float64
		if evalDurationNS > 0 {
			tps = float64(evalTokens) / (float64(evalDurationNS) / 1e9)
		}
		fmt.Printf("%s🔢 tokens: in %d  |  out %d  |  total %d  (%.1f tok/s)%s\n", dim, promptTokens, evalTokens, totalTokens, tps, reset)
		fmt.Fprintf(outFile, "🔢 tokens: in %d  |  out %d  |  total %d  (%.1f tok/s)\n", promptTokens, evalTokens, totalTokens, tps)
	}
}

// ─────────────────────────────────────────────────────────────────
// extract subcommand
// ─────────────────────────────────────────────────────────────────

var startTagRe = regexp.MustCompile(`^<<<ooo (.*) ooo>>>$`)
var endTagRe = regexp.MustCompile(`^<<<xxx (.*) xxx>>>$`)

func extractUsage() {
	fmt.Println("Usage: ola extract <input_file>")
	fmt.Println()
	fmt.Println("Extracts content between <<<ooo FILENAME ooo>>> and <<<xxx FILENAME xxx>>> markers")
	fmt.Println("in <input_file> and writes each block out to FILENAME on disk.")
	fmt.Println()
	fmt.Println("รับ argument เดียวเท่านั้น (path ของไฟล์ที่มี marker) ผิดจากนี้จะแสดง error")
	fmt.Println()
	fmt.Println("พฤติกรรม:")
	fmt.Println("  - รองรับ path ที่มี directory เช่น <<<ooo src/main.rs ooo>>> จะสร้างโฟลเดอร์ src/ ให้อัตโนมัติ (mkdir -p)")
	fmt.Println("  - ไฟล์ปลายทางจะถูกเขียนทับ (truncate) ทุกครั้งที่เจอ start tag ของชื่อไฟล์นั้น รวมถึงถ้าเจอซ้ำในไฟล์เดียวกัน")
	fmt.Println("  - end tag จะปิด block ก็ต่อเมื่อชื่อไฟล์ตรงกับ start tag ปัจจุบันเท่านั้น ชื่อไม่ตรงจะถูกข้ามไปเฉยๆ")
	fmt.Println("  - บรรทัดนอก block (ก่อน start tag แรก, ระหว่าง block, หรือหลัง end tag) จะถูกละทิ้ง ไม่ถูกเขียนไปที่ไหน")
	fmt.Println("  - ชื่อไฟล์ที่พบซ้ำ (marker เดิมมากกว่า 1 ครั้ง) จะถูกทับด้วยเนื้อหาล่าสุด และแสดงเป็น \"(re-extracted)\" แทนการนับเป็นไฟล์ใหม่")
	fmt.Println("  - จบด้วยสรุปรายชื่อไฟล์ที่แตกต่างกันทั้งหมดที่ถูกสร้าง/เขียนทับในรอบนี้")
}

func cmdExtract(args []string) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		extractUsage()
		return 0
	}
	if len(args) != 1 {
		fmt.Println("Error: Invalid arguments.")
		fmt.Println("Usage: ola extract <input_file>")
		return 1
	}

	inputFile := args[0]
	info, err := os.Stat(inputFile)
	if err != nil || info.IsDir() {
		fmt.Printf("Error: Input file '%s' not found.\n", inputFile)
		return 1
	}

	fmt.Printf("Starting extraction process from '%s'...\n", inputFile)
	fmt.Println()

	f, err := os.Open(inputFile)
	if err != nil {
		fmt.Printf("Error: Input file '%s' not found.\n", inputFile)
		return 1
	}
	defer f.Close()

	var currentFile *os.File
	var currentFilename string
	inBlock := false
	var extracted []string
	seen := map[string]bool{}
	failed := false

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	closeCurrent := func() {
		if currentFile != nil {
			currentFile.Close()
			currentFile = nil
		}
	}

	for scanner.Scan() {
		line := scanner.Text()

		if m := startTagRe.FindStringSubmatch(line); m != nil {
			filename := m[1]

			dir := filepath.Dir(filename)
			if dir != "." && dir != "" {
				if err := os.MkdirAll(dir, 0755); err != nil {
					fmt.Printf("Error: could not create directory '%s': %v\n", dir, err)
					failed = true
					continue
				}
			}

			closeCurrent()
			nf, err := os.Create(filename)
			if err != nil {
				fmt.Printf("Error: could not create file '%s': %v\n", filename, err)
				failed = true
				inBlock = false
				continue
			}
			currentFile = nf
			currentFilename = filename

			if !seen[filename] {
				seen[filename] = true
				extracted = append(extracted, filename)
				fmt.Printf("  [%02d] \u2713 %s\n", len(extracted), filename)
			} else {
				fmt.Printf("       \u21bb %s (re-extracted)\n", filename)
			}

			inBlock = true
			continue
		}

		if m := endTagRe.FindStringSubmatch(line); m != nil {
			if inBlock && m[1] == currentFilename {
				inBlock = false
				closeCurrent()
			}
			continue
		}

		if inBlock && currentFile != nil {
			fmt.Fprintln(currentFile, line)
		}
	}
	closeCurrent()

	if err := scanner.Err(); err != nil {
		fmt.Printf("Error: Extraction failed during processing: %v\n", err)
		return 1
	}

	fmt.Println()
	if len(extracted) == 0 {
		fmt.Println("Warning: No files were extracted (no valid markers found).")
	} else {
		fmt.Println("===== EXTRACTED FILES (copy block) =====")
		for _, name := range extracted {
			fmt.Println(name)
		}
		fmt.Println("========================================")
		fmt.Printf("Total: %d unique file(s) extracted.\n", len(extracted))
	}

	if failed {
		fmt.Println("Error: Extraction failed during processing.")
		return 1
	}

	fmt.Println()
	fmt.Println("Extraction completed successfully. Files have been overwritten with fresh content.")
	return 0
}
