# ola

**ola** เป็น CLI (คำสั่งเดียว, ไบนารีเดียว, เขียนด้วย Go ล้วน ไม่พึ่งพา `curl`/`jq`/`perl`/`base64` ภายนอก) สำหรับคุยกับ LLM ผ่าน Ollama (หรือ endpoint แบบ OpenAI-compatible ใดก็ได้) พร้อม **tool calling ที่เปิดใช้งานเสมอ** — โมเดลไม่ได้แค่ตอบข้อความ แต่ *อ่าน/เขียน/แก้ไฟล์จริงบนดิสก์*, รันคำสั่ง shell, ค้นเว็บ, เรียก API, โอนไฟล์ข้าม host, และถามคุณกลับเมื่อจำเป็น ทั้งหมดนี้ sandbox อยู่ใน current directory ที่คุณรัน `ola` เท่านั้น

โปรเจกต์นี้มีหกคำสั่งย่อย:

| คำสั่ง | ใช้เมื่อไหร่ |
|---|---|
| [`ola ask`](#ola-ask) | ถามคำถามครั้งเดียว มี human คอยตอบโต้ระหว่างทางได้ (เหมือนคุยกับ AI assistant ทั่วไป แต่มันแก้ไฟล์ให้จริง) |
| [`ola coding`](#ola-coding) | ให้ทำงานยาว ๆ แบบไม่มีคนเฝ้า: ป้อนไฟล์ requirements แล้วปล่อยให้มันวางแผน → เขียนโค้ด → build/test เอง → แก้จนผ่านจริง |
| [`ola telegrambot`](#ola-telegrambot) | รัน ola เป็น Telegram bot แบบ long-running ตอบเฉพาะ user/group ที่กำหนดไว้ล่วงหน้า ใช้ toolset แบบ read-only (ไม่แตะไฟล์/shell) พร้อมความจำต่อแชทที่ persist ข้าม process |
| [`ola discordbot`](#ola-discordbot) | Discord counterpart ของ `telegrambot` — หลักการ/toolset/ความจำเหมือนกันทุกประการ ต่างแค่วิธีเชื่อมต่อกับแพลตฟอร์ม (Gateway WebSocket แทน long-polling) |
| [`ola linebot`](#ola-linebot) | LINE counterpart ของ `telegrambot`/`discordbot` — หลักการ/toolset/ความจำเหมือนกันทุกประการ ต่างแค่วิธีเชื่อมต่อ (รับ webhook เข้ามาแทนที่จะ poll/เปิด connection ออกไปเอง) |
| [`ola webbot`](#ola-webbot) | เว็บแอปแชทหน้าเดียวฝังในตัวไบนารี toolset เดียวกับสามบอทข้างบน แต่ไม่มี allowlist ผู้ใช้ (ป้องกันด้วย network binding + token แทน) และไม่ persist context ลงดิสก์เลย |

> ทั้งหกคำสั่งพูดได้สองภาษา (protocol): Ollama's native `/api/chat` (ค่าเริ่มต้น) หรือ endpoint แบบ OpenAI chat-completions (`-P openai`) — ดู [Provider](#provider-ollama-vs-openai-compatible)

---

## สารบัญ

1. [ภาพรวมและปรัชญาการออกแบบ](#ภาพรวมและปรัชญาการออกแบบ)
2. [การติดตั้ง](#การติดตั้ง)
3. [เริ่มต้นใช้งานอย่างเร็ว](#เริ่มต้นใช้งานอย่างเร็ว)
4. [ตัวแปรสภาพแวดล้อม (Environment Variables) ทั้งหมด](#ตัวแปรสภาพแวดล้อม-environment-variables-ทั้งหมด)
5. [`ola ask`](#ola-ask)
6. [`ola coding`](#ola-coding)
7. [`ola telegrambot`](#ola-telegrambot)
8. [`ola discordbot`](#ola-discordbot)
9. [`ola linebot`](#ola-linebot)
10. [`ola webbot`](#ola-webbot)
11. [Provider: ollama vs openai-compatible](#provider-ollama-vs-openai-compatible)
12. [Web search / web fetch](#web-search--web-fetch)
13. [ตั้งค่า SearXNG ด้วย `websearch.yml`](#ตั้งค่า-searxng-ด้วย-websearchyml)
14. [Skills system](#skills-system)
15. [scp_copy — โอนไฟล์ข้าม host](#scp_copy--โอนไฟล์ข้าม-host)
16. [api_request — เรียก HTTP API](#api_request--เรียก-http-api)
17. [Quiet mode](#quiet-mode)
18. [ntfy.sh push notifications](#ntfysh-push-notifications)
19. [ไฟล์แพลตฟอร์ม (`platform_linux.go` / `platform_other.go`)](#ไฟล์แพลตฟอร์ม)
20. [การรันเทสต์](#การรันเทสต์)
21. [ข้อจำกัด/สิ่งที่ควรรู้](#ข้อจำกัดสิ่งที่ควรรู้)

---

## ภาพรวมและปรัชญาการออกแบบ

- **Tool calling ไม่มีสวิตช์ปิด** — ทุก request ที่ส่งไป Ollama แนบ tool schema ไปด้วยเสมอ แล้ววนลูปเรียกโมเดล → รัน tool ที่โมเดลขอ → ป้อนผลลัพธ์กลับ จนกว่าโมเดลจะตอบเป็นข้อความปกติ (หรือชนเพดานจำนวนรอบ)
- **ไม่มี `--workdir`** — ทุก path ที่ tool อ้างอิงคือ current directory ที่รัน `ola` เสมอ และไม่มีทางหลุดออกไปนอก directory นั้นได้ (ทั้ง absolute path และ `..` จะถูกปฏิเสธ)
- **เขียนไฟล์จริง ไม่ใช่ข้อความให้ copy-paste** — ola รุ่นเก่าเคยมีกลไก marker พิเศษ (`<<<ooo FILENAME ooo>>> ... <<<xxx FILENAME xxx>>>`) กับคำสั่งย่อย `extract` ให้มนุษย์ค่อยแยกไฟล์เอาทีหลัง กลไกนั้นถูกถอดออกไปแล้ว — ตอนนี้ `write_file`/`edit_file` แก้ไฟล์บนดิสก์ทันที
- **System prompt คงที่ ตายตัวในไบนารี** — ไม่มี `-s/--system` ให้เปลี่ยนจากภายนอกอีกต่อไป เพราะ contract ของ tool calling (tool มีอะไรบ้าง, sandbox ยังไง, เมื่อไหร่ต้องถาม user) สำคัญเกินกว่าจะให้ override แบบเสี่ยง prompt พังตอนรันจริง ข้อยกเว้นเดียวคือส่วน "AVAILABLE SKILLS" ที่ *เติมต่อ* ท้าย prompt เมื่อตั้งค่า skills เท่านั้น (ดู [Skills system](#skills-system))
- **ไม่เชื่อคำพูดโมเดิลเปล่า ๆ** — เมื่อโมเดิลแก้ไฟล์โค้ด ola จะรัน build/test ของโปรเจกต์เองอย่างอิสระอีกครั้งก่อนยอมรับว่า "เสร็จ" (`ola ask`) หรือบังคับ gate หลายชั้น (`ola coding`) แทนที่จะเชื่อว่าโมเดิลพูดว่า "compiles/passes tests" แล้วจบ
- **โครงสร้างซอร์สโค้ด** — โปรเจกต์รวมไฟล์ทั้งหมดเหลือน้อยไฟล์ (file-count cleanup): `main.go` (entry point + tool-calling loop ของทั้งหกคำสั่งย่อย + integrations ทั้งหมด รวม `telegrambot`/`discordbot`/`linebot`/`webbot`), `main_test.go` (เทสต์ทั้งหมด), และ `platform_linux.go`/`platform_other.go` ที่แยกเฉพาะโค้ดที่ผูกกับ build tag (`//go:build linux` / `//go:build !linux`) เพราะไฟล์แบบ build-tag ต้องมี "เฉพาะ" โค้ดที่ตรงเงื่อนไขเท่านั้น เลยรวมเข้า `main.go` ไม่ได้
- **`telegrambot`/`discordbot`/`linebot`/`webbot` เป็น trust model คนละแบบกับ `ask`/`coding`** — สองคำสั่งนั้นรันในเทอร์มินัลของผู้ดำเนินการเอง แต่แชทบอททั้งสี่รับข้อความจากคนอื่นผ่านอินเทอร์เน็ต (หรือเครือข่ายท้องถิ่น กรณี `webbot`) จึงมี toolset แบบ read-only เท่านั้น (ไม่มี `read_file`/`write_file`/`run_command`/ฯลฯ) และ dispatcher tool ของตัวเอง (`dispatchChatToolCall`) แยกจาก `ask`/`coding` โดยเจตนา — ดู [`ola telegrambot`](#ola-telegrambot)/[`ola discordbot`](#ola-discordbot)/[`ola linebot`](#ola-linebot)/[`ola webbot`](#ola-webbot)
- **`telegrambot`/`discordbot`/`linebot`/`webbot` ใช้ core ร่วมกัน** — persona, ฐานความรู้ (grep + embedding search), context compaction, grounding marker กัน hallucination, และ tool-calling loop ล้วนอยู่ใน `chatBotCore` ที่ทั้งสี่บอท embed ใช้ร่วมกัน มีแค่การเชื่อมต่อ/รับส่งข้อความกับแพลตฟอร์มเท่านั้นที่ต่างกันจริงๆ (และ `webbot` ไม่เรียก `.save()` เลย เพราะตั้งใจไม่ persist ลงดิสก์ — ดู [`ola webbot`](#ola-webbot)) — บั๊กที่แก้ในส่วน core จะได้ประโยชน์กับทุกบอทพร้อมกันเสมอ
- **ต้องมี `github.com/gorilla/websocket` เป็น Go dependency เดียว** — เพราะ Discord ไม่มี long-polling ต้องเปิด WebSocket connection ค้างไว้ (Gateway) ซึ่ง Go stdlib ไม่มี WebSocket client ให้ใช้ ส่วนที่เหลือทั้งหมด (Gateway protocol เอง, REST client, ทุก integration อื่น) ยังเขียนด้วย stdlib ล้วนเหมือนเดิม — ดู [`ola discordbot`](#ola-discordbot) สำหรับรายละเอียดการตัดสินใจนี้

---

## การติดตั้ง

**ข้อกำหนด:** Go **1.26.2** ขึ้นไป (ตาม `go.mod`) — เกือบทั้งหมด stdlib ล้วน มี Go module dependency ภายนอกอยู่ **ตัวเดียว**: `github.com/gorilla/websocket` (ใช้เฉพาะโดย `ola discordbot` สำหรับต่อ WebSocket Gateway — Go stdlib ไม่มี WebSocket client ให้ใช้ ดู [`ola discordbot`](#ola-discordbot) สำหรับเหตุผลเต็ม) แปลว่าต้องมีอินเทอร์เน็ตตอน `go build`/`go mod tidy` ครั้งแรก (หรือ vendor ไว้ล่วงหน้า) แต่หลังจากนั้น build ได้ตามปกติ ตัว `ola` binary เองใช้งานได้โดยไม่ต้องพึ่งอะไรเพิ่ม ยกเว้นสอง feature ที่พึ่ง system binary ภายนอกตอน**รัน**จริง (ไม่ใช่ตอน build): `scp_copy` (opt-in, ต้องตั้งค่าก่อนถึงจะเปิดใช้) ต้องมี `scp`, ส่วนการอ่าน PDF (ทั้งแนบผ่าน `[files...]` และ tool `read_pdf` ที่เปิดเสมอไม่ต้องตั้งค่า) ต้องมี `pdftoppm` (แพ็กเกจ `poppler-utils`) — ดู [scp_copy](#scp_copy--โอนไฟล์ข้าม-host) และ [กลไกอ่าน PDF](#กลไกอ่าน-pdf)

```bash
# 1) เตรียมโฟลเดอร์โปรเจกต์ (main.go, main_test.go, go.mod, go.sum, platform_linux.go, platform_other.go ต้องอยู่ที่เดียวกัน)
cd /path/to/ola

# 2) ดึง dependency ตัวเดียว (ครั้งแรกเท่านั้น ต้องมีอินเทอร์เน็ต)
go mod download

# 3) build เป็นไบนารีชื่อ ola
go build -o ola .

# 4) (แนะนำ) ย้ายเข้า PATH
sudo mv ola /usr/local/bin/ola
# หรือสำหรับ user เดียว:
mv ola ~/.local/bin/ola   # ต้องแน่ใจว่า ~/.local/bin อยู่ใน $PATH แล้ว
```

> เนื่องจาก `module ola` ใน `go.mod` ไม่ใช่ import path แบบ `github.com/...` คำสั่ง `go install` แบบดึงจาก remote จะใช้ไม่ได้ — ต้อง clone/copy ซอร์สมาไว้ในเครื่องแล้ว `go build` เองตามด้านบน

**ข้ามแพลตฟอร์ม:** `ola` build ได้ทั้ง Linux และ non-Linux (macOS/Windows/BSD) เพราะมีไฟล์ fallback แยกตาม build tag (`platform_linux.go` / `platform_other.go`) แต่เป้าหมายหลักของโปรเจกต์คือ **Linux** — บน non-Linux, `run_command`/`coding` ยังทำงานได้แต่ killed process จะ kill ได้แค่ตัวลูกโดยตรง ไม่ใช่ทั้ง process group (ดู [ไฟล์แพลตฟอร์ม](#ไฟล์แพลตฟอร์ม))

---

## เริ่มต้นใช้งานอย่างเร็ว

`ola` ต้องมี Ollama รันอยู่ (default: `http://localhost:11434`) และต้องระบุโมเดล อย่างน้อยหนึ่งในสองวิธี:

```bash
# วิธีที่ 1: ตั้ง environment variable ไว้ครั้งเดียว (แนะนำสำหรับใช้งานประจำ)
export OLA_OLLAMA_MODEL=qwen3.6:27b
ola ask "สรุปไฟล์นี้ให้หน่อย" README.md

# วิธีที่ 2: ระบุโมเดลทุกครั้งด้วย -m
ola ask -m qwen3.6:27b "review โค้ดนี้ให้หน่อย" main.py
```

ถ้าไม่แนบไฟล์ใด ๆ เลย ola จะสแกน current directory ทั้งหมด (recursive, ยกเว้น `.git`/`node_modules`/`vendor`/build artifact ต่าง ๆ และไฟล์ binary ส่วนใหญ่ — แต่ `.pdf` ยังโผล่ในรายชื่อ เพราะมี tool `read_pdf` อ่านได้ ดู [กลไกอ่าน PDF](#กลไกอ่าน-pdf)) แล้วแปะ directory tree เข้า prompt แรกให้อัตโนมัติ โมเดิลจะเห็น scope ของโปรเจกต์ทันทีโดยไม่ต้องเสีย tool-call รอบแรกไปกับการ `search_files('*')` สำรวจเปล่า ๆ:

```bash
cd ~/projects/my-api
ola ask "หาว่าโปรเจกต์นี้ใช้ framework อะไร แล้วเพิ่ม health check endpoint ให้"
```

---

## ตัวแปรสภาพแวดล้อม (Environment Variables) ทั้งหมด

ทุกตัวแปรมี flag ที่ override ได้เสมอ (flag ชนะ env เสมอ) ใช้ร่วมกันได้ทั้ง `ola ask` และ `ola coding`

### การเชื่อมต่อ / โมเดล

| ตัวแปร | Flag | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `OLA_PROVIDER` | `-P, --provider` | `ollama` | `ollama` หรือ `openai` — ดู [Provider](#provider-ollama-vs-openai-compatible) |
| `OLA_OLLAMA_API_BASE` | `--api-base` | `http://localhost:11434` | ใช้เมื่อ provider เป็น `ollama` |
| `OLA_OLLAMA_API_KEY` | `-k, --key` (เปิดใช้) | — | Bearer token, ใช้เมื่อ provider เป็น `ollama` |
| `OLA_OLLAMA_MODEL` | `-m, --model` | — | **จำเป็น** ถ้าไม่ใช้ `-m` และ provider เป็น `ollama` |
| `OLA_OLLAMA_CONTEXT_SIZE` | `-c, --ctx` | `16384` | `num_ctx` ต่อ request — ไม่มีผลเมื่อ provider เป็น `openai` |
| `OLA_OPENAI_API_BASE` | `--api-base` | `http://localhost:11434/v1` | ใช้เมื่อ provider เป็น `openai` |
| `OLA_OPENAI_API_KEY` | `-k, --key` (เปิดใช้) | — | Bearer token, ใช้เมื่อ provider เป็น `openai` |
| `OLA_OPENAI_MODEL` | `-m, --model` | — | ใช้เมื่อ provider เป็น `openai` |

### Output / notification

| ตัวแปร | Flag | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `OLA_OUTPUT_FILE` | `-o, --output` | `output.txt` | log แบบเต็ม (เขียนทับเสมอ เว้นแต่ `-a/--append`) |
| `OLA_TOPIC` | `-x, --topic` | — | topic สำหรับ ntfy.sh — ดู [ntfy.sh](#ntfysh-push-notifications) |
| `OLA_QUIET` | `-q, --quiet` | ปิด | รับค่า `1`/`true`/`yes`/`on` (ไม่สนตัวพิมพ์เล็ก-ใหญ่) — ดู [Quiet mode](#quiet-mode) |

### Web search / fetch (opt-in)

| ตัวแปร | Flag | ค่าเริ่มต้น |
|---|---|---|
| `OLA_OLLAMA_SEARCH_API_KEY` (fallback: `$OLLAMA_API_KEY`) | `--ollama-search-key` | — |
| `OLA_OLLAMA_SEARCH_API_BASE` | — | `https://ollama.com` |
| `OLA_SEARXNG_API_BASE` | `--searxng-url` | — (ถ้าตั้งคู่กับ Ollama key ด้านบน **SearXNG ชนะเสมอ**) |
| `OLA_SEARCH_MAX_RESULTS` | `--search-max-results` | `5` |
| `OLA_SEARCH_CONCURRENCY` | `--search-concurrency` | `3` |
| `OLA_FETCH_CONCURRENCY` | `--fetch-concurrency` | `4` |
| `OLA_SEARCH_TIMEOUT_SEC` | `--search-timeout` | `20` |
| `OLA_FETCH_TIMEOUT_SEC` | `--fetch-timeout` | `30` |

`web_fetch` เปิดอัตโนมัติเสมอโดยไม่ต้องตั้งค่าอะไร ปิดได้ทางเดียวคือ `--no-web-search` (ปิดทั้ง `web_search` และ `web_fetch` พร้อมกัน)

### Skills (opt-in)

| ตัวแปร | Flag |
|---|---|
| `OLA_SKILLS_DIR` | `--skills-dir` |

### scp_copy (opt-in)

| ตัวแปร | Flag | ค่าเริ่มต้น |
|---|---|---|
| `OLA_SCP_HOSTS` | `--scp-hosts` | — (ไม่ตั้ง = ไม่มี tool นี้เลย) |
| `OLA_SCP_LOCAL_DIR` | `--scp-local-dir` | current directory |
| `OLA_SCP_KEY` | `--scp-key` | ใช้ ssh-agent/`~/.ssh/config` |
| `OLA_SCP_TIMEOUT_SEC` | `--scp-timeout` | `120` |
| `OLA_SCP_MAX_BYTES` | `--scp-max-bytes` | `104857600` (100MB) |

### api_request (opt-in)

| ตัวแปร | Flag | ค่าเริ่มต้น |
|---|---|---|
| `OLA_API_ENDPOINTS` | `--api-endpoints` | — |
| `OLA_API_ENDPOINT_<ALIAS>_AUTH_HEADER` / `_AUTH_VALUE` | — | credential เฉพาะ endpoint (ola แนบให้เอง โมเดิลไม่เห็นค่าจริง) |
| `OLA_API_ALLOW_DIRECT_URL` | `--api-allow-direct-url` | ปิด |
| `OLA_API_ALLOW_MUTATING` | `--api-allow-mutating` | ปิด |
| `OLA_API_REQUEST_TIMEOUT_SEC` | `--api-timeout` | `30` |

### อ่าน PDF (`[files...]` + tool `read_pdf` — ต้องมี `pdftoppm` ติดตั้ง)

| ตัวแปร | Flag | ค่าเริ่มต้น |
|---|---|---|
| `OLA_PDF_MAX_PAGES` | `--pdf-max-pages` | `20` |
| `OLA_PDF_DPI` | `--pdf-dpi` | `150` |

---

## `ola ask`

```
Usage: ola ask [options] <prompt> [files...]
       ola ask [options] -f <prompt-file> [files...]
```

### Tool พื้นฐาน 10 ตัว (มีเสมอ ไม่มีเงื่อนไข)

| Tool | หน้าที่ |
|---|---|
| `read_file` | อ่านไฟล์ทั้งไฟล์ |
| `search_files` | หาไฟล์ด้วย glob pattern, กรองด้วย grep query ได้ |
| `write_file` | สร้าง/เขียนทับไฟล์ทั้งไฟล์ |
| `edit_file` | ค้น/แทนที่แบบ unique ในไฟล์ที่มีอยู่แล้ว |
| `create_folder` | สร้างโฟลเดอร์ (รวม parent ที่ยังไม่มี) |
| `ask_user` | หยุดรอถามผู้ใช้ผ่าน stdin |
| `get_current_time` | เวลาจริงจากระบบ ระบุ IANA timezone ได้ |
| `delay` | หยุดรอตามระยะเวลารูปแบบ `XdXhXmXs` (สูงสุด 24 ชม./ครั้ง) |
| `run_command` | รันคำสั่ง shell ใด ๆ จาก current directory — เปิดเสมอ ไม่ขึ้นกับ toolchain หรือ flag ใด ๆ แต่ทุกคำสั่งต้องผ่าน denylist และขอบเขต working-directory-only ก่อน (ดู [กลไก run_command](#กลไก-run_command-เปิดเสมอ)) |
| `read_pdf` | อ่านไฟล์ PDF จาก path ใดก็ได้ (relative กับ current directory) แล้วแปลงเป็นภาพส่งกลับมาให้ — โมเดิลเรียกเองได้ทุกเมื่อระหว่างเซสชัน ไม่จำเป็นต้องแนบไฟล์มาตอนเริ่มผ่าน `[files...]` (ดู [กลไกอ่าน PDF](#กลไกอ่าน-pdf)) |

### Tool แบบมีเงื่อนไข (เปิดเมื่อ config ตรงเงื่อนไขเท่านั้น)

- **`web_search`** / **`web_fetch`** — `web_fetch` เปิดเสมอ, `web_search` เปิดเมื่อ config backend ใดบ้างหนึ่ง (ดู [Web search](#web-search--web-fetch))
- **`read_skill`** — เปิดเมื่อตั้ง `--skills-dir`/`OLA_SKILLS_DIR` (ดู [Skills](#skills-system))
- **`scp_copy`** — เปิดเมื่อตั้ง `--scp-hosts`/`OLA_SCP_HOSTS`
- **`api_request`** — เปิดเมื่อตั้ง `--api-endpoints`/`OLA_API_ENDPOINTS` หรือ `--api-allow-direct-url`

### กลไก `run_command` (เปิดเสมอ)

`run_command` อยู่ในรายการ tool เสมอ ไม่ว่า current directory จะมี toolchain ที่รู้จักหรือไม่ และไม่ขึ้นกับ `-V/--no-verify` — flag นั้นคุมเฉพาะกลไก auto-verify ด้านล่าง ไม่ใช่ตัว `run_command` เอง ทุกคำสั่งที่โมเดิลส่งเข้ามาต้องผ่าน 2 ชั้นตรวจสอบก่อนรันจริงทุกครั้ง:

1. **Denylist** — คำสั่งที่ทำลายไฟล์ระบบหรือมีผลกว้างระดับ host ถูกปฏิเสธเสมอ: `rm`, `rmdir`, `dd`, `shred`, `wipefs`, `mkswap`, `fdisk`, `parted`, `format`, `mkfs*`, `shutdown`, `reboot`, `poweroff`, `halt`, `init`, `mount`, `umount`, `systemctl`, `service`, `sudo`, `su`, `doas`, `passwd`, `useradd`/`userdel`/`usermod`, `groupadd`/`groupdel`, `visudo`, `iptables`/`ip6tables`, `ufw`, `firewall-cmd`, `killall`, `pkill`, `crontab` — ตรวจทุก segment ที่คั่นด้วย `&&`/`||`/`;`/`|` ไม่ใช่แค่คำสั่งแรก (เช่น `go build ./... && rm -rf .` ก็ถูกบล็อก) `kill` เฉย ๆ (ไม่ใช่ `killall`/`pkill`) ยังใช้ได้ปกติ เพราะเป็นการหยุด process ที่ session นี้เองสั่งรันไว้
2. **ขอบเขต working directory** — ห้ามอ้างอิง absolute path ที่อยู่นอก current directory (ยกเว้น `/dev/null`/`/dev/zero`/`/dev/stdin`/`/dev/stdout`/`/dev/stderr`/`/dev/urandom`/`/dev/random` ซึ่งเป็น I/O target มาตรฐาน) และห้ามใช้ `..` หลุดออกนอก directory — URL อย่าง `https://...` ไม่นับเป็น absolute path จึงยังเรียก `curl`/`git clone`/`go get` ได้ตามปกติ **การรัน binary ที่เพิ่ง build เสร็จในไดเรกทอรีปัจจุบันเพื่อทดสอบ (เช่น `./mathcalc`) ใช้ path แบบ relative ได้ตามปกติเสมอ** ไม่ว่าจะพิมพ์แบบ `./ชื่อไฟล์` หรือ path แบบ absolute ที่ resolve แล้วชี้เข้าไปในไดเรกทอรีเดียวกันจริง ๆ (รวมถึงกรณี working directory เข้าถึงผ่าน symlink ด้วย) — ถ้าโมเดิลเจอ error นี้ ข้อความ error จะแนะนำ path แบบ relative ที่ถูกต้องให้เองด้วย

ทั้งสองชั้นเป็นการตรวจ **แบบ text/regex เท่านั้น ไม่ใช่ sandbox จริง** (มองทะลุ command substitution `$(...)`/`` `...` `` หรือ script ที่ถูกเรียกโดยชื่อไม่ได้) — ออกแบบมาเพื่อกันความผิดพลาดที่พบบ่อยและเสียหายมาก (`rm -rf` พลาด, absolute path หลุด, `cd ..` หลงทาง) ไม่ใช่การการันตีความปลอดภัยสมบูรณ์ต่ออินพุตที่จงใจหลบเลี่ยง — เครื่องหมาย `/` ที่เป็นตัวดำเนินการหาร (เช่นรันโปรแกรมคำนวณด้วย `./math-tool "10 / 0"`) ไม่นับเป็น absolute path เพราะ `/` เดี่ยวๆ ที่ไม่มีอะไรต่อท้ายก่อนช่องว่าง/เครื่องหมายคำพูด ไม่ถือเป็น path จริง (path จริงจะมีอะไรต่อท้าย `/` เสมอ เช่น `/etc/passwd`)

### กลไก Auto-verify หลังแก้โค้ด (เปิดอัตโนมัติ, ปิดได้ด้วย `-V/--no-verify`)

แยกจากเรื่อง `run_command` ด้านบนโดยสิ้นเชิง (`run_command` เปิดเสมอไม่ว่า flag นี้จะเป็นอย่างไร) — ถ้า current directory มี toolchain ที่รู้จัก (`go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`/`requirements.txt`/`setup.py`, `Makefile`) **และ**โมเดิลแก้ไฟล์ (`write_file`/`edit_file`) ในเซสชันนี้ ก่อนตอบจบ ola จะรัน build/test ของโปรเจกต์เองอีกครั้งอย่างอิสระ (ไม่เชื่อคำโมเดิลเปล่า ๆ) ถ้าไม่ผ่านจะป้อนผลลัพธ์กลับให้โมเดิลแก้ต่อ **สูงสุด 3 รอบ** ก่อนหยุดให้ผู้ใช้ตรวจสอบเอง — trigger เฉพาะเมื่อไฟล์ที่แก้เป็น source file ของ toolchain จริง ๆ (แก้ `.md`/`.txt` ไม่ trigger) ถ้าไม่มี toolchain ที่รู้จัก หรือใช้ `-V/--no-verify` จะไม่มี auto-verify รอบนี้เลย (โมเดิลยังเรียก `run_command` เองได้ตามปกติ เพียงแต่ ola จะไม่ auto-verify ซ้ำให้)

### ตัวเลือกทั้งหมด

```
  -m, --model <n>       โมเดลที่ใช้ [จำเป็นถ้าไม่ตั้ง $OLA_OLLAMA_MODEL/$OLA_OPENAI_MODEL]
  -c, --ctx <num>       num_ctx ต่อ request (default: 16384; ไม่มีผลกับ openai)
  -k, --key             ส่ง Authorization: Bearer จาก env key ของ provider ที่เลือก
  -P, --provider <p>    "ollama" (default) หรือ "openai"
      --api-base <url>  override host ของ provider ที่เลือกอยู่
  -T, --no-think        ปิด thinking mode (ส่ง "think": false)
  -x, --topic <topic>   ส่ง notification ไป ntfy.sh
  -o, --output <file>   บันทึกผลลัพธ์+log ลงไฟล์ (default: output.txt, เขียนทับเสมอ)
  -a, --append          ต่อท้ายไฟล์ output แทนเขียนทับ
  -q, --quiet           Quiet mode — ดูหัวข้อ Quiet mode
  -r, --raw             ไม่ใส่ separator ระหว่างไฟล์แนบ
  -f, --prompt-file <f> อ่าน prompt จากไฟล์แทนพิมพ์เป็น argument
  -n, --dry-run         แสดง JSON payload รอบแรก + system prompt โดยไม่เรียก API จริง
  -V, --no-verify       ปิด auto-verify หลังแก้โค้ด (ไม่มีผลต่อ run_command เอง — เปิดใช้งานเสมอไม่ว่าจะตั้ง flag นี้หรือไม่)
      --cmd-timeout <sec>       timeout ต่อ run_command หนึ่งครั้ง (default: 60)
      --ollama-search-key <k>  เปิด web_search ผ่าน Ollama hosted API
      --searxng-url <u>        เปิด web_search ผ่าน SearXNG (ชนะ Ollama key ถ้าตั้งคู่กัน)
      --no-web-search           ปิดทั้ง web_search และ web_fetch
      --skills-dir <list>       เปิด read_skill (comma-separated หลาย directory ได้)
      --scp-hosts <list>        เปิด scp_copy
      --scp-local-dir/-key/-timeout/-max-bytes
      --api-endpoints <list>    เปิด api_request
      --api-allow-direct-url / --api-allow-mutating / --api-timeout
      --pdf-max-pages <n>       จำนวนหน้าแรกสูงสุดที่แปลงเป็นภาพต่อไฟล์ PDF (default: 20)
      --pdf-dpi <n>             ความละเอียดตอนแปลง PDF เป็นภาพ (default: 150)
  -h, --help             แสดงข้อความช่วยเหลือนี้
```

### ไฟล์แนบ (`[files...]`)

- `.jpg .jpeg .png .webp .gif` → อ่านและแนบเป็น base64 ใน field `images` ของ user message
- `.pdf` → แปลงเป็นภาพทีละหน้า (ผ่าน `pdftoppm` จาก poppler-utils) แล้วแนบเป็น `images` แบบเดียวกับรูป — ดู [กลไกอ่าน PDF](#กลไกอ่าน-pdf) ด้านล่าง
- นามสกุลอื่นทั้งหมด → อ่านเป็นข้อความต่อท้ายเข้าไปใน prompt โดยตรง (คั่นด้วย separator เว้นแต่ใช้ `-r/--raw`)
- ไฟล์ที่ไม่พบ → แสดง warning แล้วข้าม ไม่หยุดโปรแกรม

### กลไกอ่าน PDF

`ola` อ่าน PDF ได้ 2 ทาง ที่ใช้กลไกแปลงไฟล์เดียวกันทุกประการ (ต่างกันแค่ *จังหวะ* ที่เกิดขึ้น):

1. **แนบผ่าน `[files...]` ตอนเริ่มเซสชัน** (ดูหัวข้อด้านบน) — เหมาะกับไฟล์ที่รู้อยู่แล้วว่าต้องใช้ตั้งแต่ต้น
2. **tool `read_pdf(path)`** — โมเดิลเรียกได้เอง**ระหว่าง**เซสชัน สำหรับไฟล์ PDF ที่เพิ่งเจอระหว่างทำงาน (เช่นจาก `search_files`, จาก directory listing, หรือผู้ใช้แค่พิมพ์ชื่อไฟล์มาเฉย ๆ โดยไม่ได้แนบ) อยู่ใน [Tool พื้นฐาน 10 ตัว](#tool-พื้นฐาน-10-ตัว-มีเสมอ-ไม่มีเงื่อนไข) เสมอ ไม่ต้องตั้งค่าอะไรเพิ่ม path ที่รับเป็น relative กับ current directory และ sandbox เดียวกับ `read_file`/`write_file` (path ที่หลุดออกนอก current directory จะถูกปฏิเสธ) ผลลัพธ์ของ `read_pdf` ตัว string จะบอกแค่จำนวนหน้าที่แปลงได้ — **ภาพจริงจะมาในข้อความถัดไป** (ola แทรก message แบบ `role: "user"` ต่อท้ายให้อัตโนมัติ ไม่ได้แนบอยู่ใน tool result โดยตรง เพราะ tool-result message ไม่ใช่ที่ที่รับประกันว่าภาพจะไปถึงโมเดลได้ในทุก provider)

`.pdf` ยังคงเป็น binary ในสายตาของ `looksBinaryFile` (ไม่ถูกอ่านเป็นข้อความหรือ grep เนื้อหา) แต่**ถูกยกเว้นไว้ให้ยังแสดงชื่อไฟล์**ทั้งใน directory tree ที่แปะเข้า prompt แรกอัตโนมัติ และผลลัพธ์ของ `search_files` (ไม่เหมือนไฟล์ binary อื่นอย่าง `.png`/`.zip` ที่ถูกซ่อนไปเลย) — เพื่อให้โมเดิลรู้ว่ามี PDF อยู่ตั้งแต่ต้น ไม่ต้องเดาหรือให้ผู้ใช้บอกชื่อไฟล์ก่อนถึงจะเรียก `read_pdf` ได้ ถ้าโมเดิลพลาดเรียก `read_file` กับไฟล์ `.pdf` เข้า จะได้ error แนะนำให้ไปใช้ `read_pdf` แทนทันที

`ola` ไม่มีตัวอ่าน PDF ในตัว (Go standard library ไม่มี) จึงเรียกโปรแกรมภายนอก **`pdftoppm`** (จาก poppler-utils) มาแปลงแต่ละหน้าของ PDF เป็นภาพ PNG ก่อนแนบเข้า request แบบเดียวกับไฟล์รูปทั่วไป — เหตุผลที่เลือกแปลงเป็น **ภาพ** แทนการดึงข้อความ (text extraction):

- ใช้ได้แม้เป็น PDF ที่ **สแกนมา** (ไม่มี text layer ฝังอยู่เลย) เพราะโมเดลอ่านเนื้อหาจากภาพโดยตรง ไม่ต้องพึ่ง text layer ในไฟล์
- **ข้อแลกเปลี่ยน:** โมเดลที่ใช้ต้องรองรับ vision ด้วย (ไม่ใช่ทุกโมเดลใน Ollama จะทำได้) — ola ไม่เช็คให้ว่าโมเดลรองรับหรือไม่ แค่ส่งภาพไปตามปกติ เหมือนที่ทำกับไฟล์รูปที่แนบอยู่แล้ว

**ติดตั้ง:** ต้องมี `pdftoppm` ใน `PATH` ก่อน (แพ็กเกจ `poppler-utils` บน Debian/Ubuntu, `poppler` บน macOS Homebrew/Arch) — ถ้าไม่พบ ola จะแสดง warning แล้วข้ามไฟล์ PDF นั้นไป (สำหรับการแนบผ่าน `[files...]`) หรือคืน error ให้โมเดิลเห็น (สำหรับ `read_pdf`) ไม่ทำให้ทั้งเซสชันล้มไม่ว่าทางไหน (เหมือน `scp_copy` ที่พึ่ง system `scp` binary อยู่แล้ว — นี่คือทางเลือกเดียวกัน: ยอมพึ่ง binary ภายนอกแทนการเพิ่ม Go dependency เข้าไปในตัว `ola` เอง)

**ควบคุมได้ด้วย (ใช้ร่วมกันทั้งสองทาง):**

| Flag | Env | Default | ความหมาย |
|---|---|---|---|
| `--pdf-max-pages <n>` | `OLA_PDF_MAX_PAGES` | 20 | จำนวนหน้าแรกสูงสุดที่แปลงต่อไฟล์ — กันไม่ให้เอกสารร้อยกว่าหน้าพยายามแนบภาพความละเอียดสูงเป็นร้อยรูปในคำขอเดียว ถ้าเอกสารมีมากกว่านี้ ola จะแนบ N หน้าแรกแล้วเติมข้อความเตือนว่าอาจมีหน้าที่ไม่ได้แนบ |
| `--pdf-dpi <n>` | `OLA_PDF_DPI` | 150 | ความละเอียดตอน rasterize — เพิ่มถ้าตัวหนังสือในเอกสารเล็ก/แน่นมาก ลดถ้าต้องการประหยัด payload/context |

ไม่มี OCR หรือการดึงข้อความใด ๆ เกิดขึ้นในกระบวนการนี้ — คุณภาพการอ่านเนื้อหาขึ้นอยู่กับความสามารถ vision ของโมเดลที่ใช้ล้วน ๆ ทั้ง `[files...]` และ `read_pdf` ใช้ `--pdf-max-pages`/`--pdf-dpi` ของเซสชันเดียวกัน จะแยกค่ากันคนละทางไม่ได้

### ตัวอย่างการใช้งาน

```bash
# ตั้งโมเดลไว้ครั้งเดียว
export OLA_OLLAMA_MODEL=qwen3.6:27b

# รีวิวโค้ดไฟล์เดียว
ola ask 'review this code' main.py

# ส่ง Authorization header + ตั้ง context ใหญ่ขึ้น + วิเคราะห์/แก้หลายไฟล์
ola ask -k -c 65536 'วิเคราะห์และแก้ไฟล์ที่เกี่ยวข้อง' src/*.py

# แจ้งเตือนผ่าน ntfy.sh เมื่องานเสร็จ/มีการแก้ไฟล์
ola ask -x mytopic 'refactor the auth module'

# prompt ยาว ๆ เก็บไว้ในไฟล์ ส่วน [files...] ทั้งหมดกลายเป็นไฟล์แนบ
ola ask -f prompt.txt src/*.go

# ใช้ OLA_TOPIC จาก environment แทนการพิมพ์ -x ทุกครั้ง
export OLA_TOPIC=mytopic
ola ask 'deploy to production'

# ดึง skill มาช่วยงานเฉพาะทาง (เช่นสร้างสไลด์)
ola ask --skills-dir /mnt/skills/public,/mnt/skills/private 'สร้างสไลด์สรุปบทที่ 5'

# สำรองไฟล์ไปยัง remote host ที่ตั้งไว้ล่วงหน้า
ola ask --scp-hosts 'backup=moo@10.0.0.5/srv/backup' 'สำรอง report.txt ไปที่ backup หน่อย'

# ให้โมเดิลเช็คสถานะ Ollama ผ่าน API ภายในของตัวเอง
ola ask --api-endpoints 'ollama=http://localhost:11434' 'เช็คว่ามีโมเดลอะไรบ้างใน ollama ตอนนี้'

# รันแบบเงียบ (terminal เหลือแค่คำตอบ), ntfy ได้แค่ ask_user + จบงาน
ola ask -q -x mytopic 'deploy to production'

# ดู payload ที่จะส่งจริง โดยไม่ยิง request จริง (ไว้ debug prompt/tool schema)
ola ask -n 'ทดสอบ dry run'

# แนบไฟล์ PDF (แปลงเป็นภาพให้โมเดล vision อ่าน - ต้องมี pdftoppm และโมเดลที่รองรับ vision)
ola ask -m qwen3-vl 'สรุปเอกสารนี้ให้หน่อย' invoice.pdf

# แนบ PDF หนา ๆ แต่จำกัดหน้า/ความละเอียดเพื่อประหยัด context
ola ask -m qwen3-vl --pdf-max-pages 5 --pdf-dpi 100 'สรุปสารบัญคร่าว ๆ' manual.pdf

# ไม่ต้องแนบไฟล์เลยก็ได้ - ให้โมเดิลหาและอ่าน PDF เองผ่าน tool read_pdf ระหว่างทาง
ola ask -m qwen3-vl 'ในโฟลเดอร์นี้มีไฟล์ PDF อะไรบ้าง ลองอ่านแล้วสรุปให้หน่อย'
```

**หมายเหตุ:**
- Tool-calling วนได้สูงสุด **25 รอบ** ต่อการรัน 1 ครั้ง ถ้าเกินจะหยุดพร้อม warning (กันลูปไม่จบ)
- `ask_user` ต้องมี stdin เป็น terminal จริง — ถ้ารันแบบ non-interactive (script/cron/pipe) แล้วโมเดิลเรียก `ask_user` จะได้ error กลับไปแทน พร้อมคำแนะนำให้ตัดสินใจเองแล้วระบุ assumption
- Exit code เป็น `1` ถ้า HTTP status ที่ตอบกลับ >= 400 (เนื้อหายังถูกแสดง/บันทึกตามปกติ)

---

## `ola coding`

```
Usage: ola coding [options]
```

คำสั่งย่อยสำหรับรันแบบ **ไม่มีคนเฝ้า**: ไม่มี prompt จาก user โดยตรง แต่อ่านไฟล์ requirements (default `requirements.md`) แล้ววางแผนเป็น task checklist → implement → เรียก build/test ของโปรเจกต์เอง → วนแก้จนกว่าจะผ่านจริง → รายงานผล

### Tool เพิ่มเติม (นอกเหนือจาก 10 ตัวของ `ask` ซึ่งรวม `run_command`/`read_pdf` อยู่แล้ว)

`add_tasks`, `mark_task_done`, `self_review_requirements`, `report_complete` — รวมถึง `web_fetch`/`web_search`/`api_request`/`read_skill` แบบมีเงื่อนไขเหมือน `ask` ทุกประการ `run_command`/`read_pdf` ใช้กฎเดียวกับที่อธิบายไว้ใน [กลไก run_command](#กลไก-run_command-เปิดเสมอ) และ [กลไกอ่าน PDF](#กลไกอ่าน-pdf) ทุกประการ — เหมาะกับ `requirements.md` ที่อ้างอิงสเปคเป็นไฟล์ PDF โดยเฉพาะ เพราะโมเดิลเรียก `read_pdf` เองได้ทันทีที่เจอ ไม่ต้องให้ผู้ใช้แนบมาก่อน

### กลไกคุมคุณภาพ 5 ชั้น (default เข้มงวดที่สุด ปรับได้ด้วย flag)

1. **หลัง `write_file`/`edit_file` ทุกครั้ง** — รัน lint + build-only check ทันที (เร็ว ไม่รอถึง `mark_task_done`) แล้วแปะผลท้าย tool result ให้โมเดิลเห็นสด ๆ → ปิดด้วย `--no-edit-verify` (สำหรับโปรเจกต์ build ช้ามาก)
2. **`mark_task_done` มี gate ในตัว** — รัน lint (`go vet`+`gofmt` / `cargo clippy` / `eslint` / `python compileall` แล้วแต่ toolchain) + build-only เสมอ ล้มเหลว = block เหมือน build fail ถ้า task นั้นมี `acceptance_check` จะรันเพิ่มด้วย ต้องผ่านทั้งหมดถึงปิด task ได้
3. **Stuck-detection** — task เดียวถูกปฏิเสธซ้ำครบ **3 ครั้งติดกัน** → ola บล็อก `mark_task_done` กับ task นั้นทันที จนกว่าจะเรียก `add_tasks` (แตกเป็น subtask เล็กลง) หรือ `ask_user` (ขอความช่วยเหลือ) ก่อน
4. **ก่อน `report_complete`** ต้องเรียก `self_review_requirements(all_requirements_met=true)` สด ๆ ก่อนเสมอ (แก้ไฟล์เพิ่มหลังจากนั้นทำให้ต้อง review ใหม่) → ปิดด้วย `--no-self-review`
5. **`report_complete` ไม่จบ session ทันที** — ola รัน lint+build+test ของโปรเจกต์เองอิสระอีกครั้งก่อน ถ้าไม่ผ่าน error จะถูกป้อนกลับเข้า conversation และ loop ทำงานต่อจนผ่านจริงหรือจนถึง cap

### Preflight check

ก่อนเริ่ม loop ola เช็คว่า binary ที่ toolchain ต้องใช้มีอยู่จริงใน `PATH` หรือไม่ — ถ้าขาดจะ error ทันทีแทนที่จะเสีย API call ไปกับ session ที่รู้อยู่แล้วว่าจะพัง (ปิดด้วย `--no-preflight`)

| ภาษา | ต้องมีใน PATH | Lint ที่ใช้ |
|---|---|---|
| go | `go`, `gofmt` | `go vet` + `gofmt -l` |
| node | `npm`/`yarn`/`pnpm`, `npx`, `node` | `npx eslint .` (เฉพาะถ้าเจอ eslint config) |
| rust | `cargo`, `rustc` | `cargo clippy` (ต้องมี component clippy) |
| python | `python3`, `pytest`, `pip` | `python3 -m compileall` (syntax check เท่านั้น) |
| make | `make` | ไม่มี lint อัตโนมัติ — ใช้ `--lint-cmd` ถ้าต้องการ |

### State/output files ที่จะถูกสร้างใน current directory

| ไฟล์ | หน้าที่ |
|---|---|
| `.ola-coding-state.json` | task checklist แบบ JSON (สำหรับ resume ข้ามการรัน) |
| `PROGRESS.md` | task checklist แบบอ่านง่าย อัปเดตทุกครั้งที่ task เปลี่ยนสถานะ |
| `ASSUMPTIONS.md` | log ของทุกครั้งที่ `ask_user` ถูกเรียก (คำถาม + คำตอบ/assumption) |

### ตัวเลือกทั้งหมด

```
  -m, --model <n>         โมเดลที่ใช้
  -c, --ctx <num>         num_ctx ต่อ request (ไม่มีผลกับ openai)
  -k, --key               ส่ง Authorization: Bearer
  -P, --provider <p>      "ollama" หรือ "openai"
      --api-base <url>    override host
  -T, --no-think          ปิด thinking mode (ไม่มีผลกับ openai)
  -x, --topic <topic>     ส่ง notification ไป ntfy.sh
  -o, --output <file>     บันทึก log ลงไฟล์
  -q, --quiet             Quiet mode
  -f, --requirements <f>  ไฟล์ requirements (default: requirements.md)
      --replan             ทิ้ง task state เดิม (.ola-coding-state.json) แล้ววางแผนใหม่
      --lint-cmd <cmd>     ระบุคำสั่ง lint เอง (override การตรวจจับอัตโนมัติ)
      --no-self-review     ปิด gate self_review_requirements (default: เปิด)
      --no-edit-verify     ปิด lint+build-check หลัง write_file/edit_file (default: เปิด)
      --no-preflight       ข้ามการเช็ค binary ใน PATH ก่อนเริ่ม (default: เช็ค)
      --max-iterations <n> เพดานรอบของ loop (default: 300)
      --max-duration <dur> เพดานเวลารวม เช่น "2h", "45m" (default: 3h)
      --cmd-timeout <sec>  timeout ต่อ run_command/lint/verify หนึ่งครั้ง (default: 120)
      (flag web_search/skills/api_request/scp เหมือน ola ask ทุกประการ)
  -n, --dry-run            แสดง JSON payload รอบแรกโดยไม่เรียก API จริง
  -h, --help                แสดงข้อความช่วยเหลือนี้
```

### ตัวอย่าง `requirements.md`

`requirements.md` เป็น **markdown ธรรมดา** ไม่มี schema บังคับ — โมเดิลอ่านเป็น prose แล้ววางแผนเอง เขียนให้ชัดเจนที่สุดเท่าที่จะทำได้ ตัวอย่าง:

```markdown
# Requirements: Todo API

สร้าง REST API สำหรับจัดการ todo list ด้วย Go + net/http (ไม่ใช้ framework ภายนอก)

## ต้องมี
- POST /todos      สร้าง todo ใหม่ (body: {"title": string})
- GET /todos       คืนรายการ todo ทั้งหมดเป็น JSON
- PATCH /todos/:id ตั้งค่า done = true/false
- DELETE /todos/:id ลบ todo
- เก็บข้อมูลใน memory ก็พอ (ไม่ต้องต่อฐานข้อมูล)
- ต้องมี unit test ครอบคลุมทั้ง 4 endpoint

## ไม่ต้องมี
- ไม่ต้องมี authentication
- ไม่ต้องมี frontend
```

### ตัวอย่างการใช้งาน

```bash
export OLA_OLLAMA_MODEL=qwen3.6:27b

# รันแบบพื้นฐาน อ่าน requirements.md ใน current directory
ola coding

# ระบุไฟล์ requirements เอง + จำกัดเวลารวมไว้ 6 ชม. + แจ้งเตือนทาง ntfy.sh
ola coding -f docs/requirements.md -x mytopic --max-duration 6h

# ใช้ lint command ของตัวเอง (เช่น golangci-lint แทน go vet เปล่า ๆ)
ola coding --lint-cmd 'golangci-lint run'

# ให้โมเดิลดึง best-practice skill มาช่วยระหว่างทำงานแบบไม่มีคนเฝ้า
ola coding --skills-dir /mnt/skills/public,/mnt/skills/private

# รันแบบเงียบ + จำกัดเวลา
ola coding -q -x mytopic --max-duration 6h

# โปรเจกต์ build ช้ามาก → ปิด per-edit check และเพิ่ม timeout ต่อคำสั่ง
ola coding --no-edit-verify --cmd-timeout 300

# ทิ้ง state เดิมแล้ววางแผนใหม่ทั้งหมด (เช่น requirements เปลี่ยนไปมาก)
ola coding --replan
```

---

## `ola telegrambot`

รัน ola เป็น Telegram bot แบบ long-running: long-poll ผ่าน `getUpdates` (ไม่ต้องมี public HTTPS/webhook หรือ TLS cert ใดๆ — ต้องการแค่ต่อ HTTPS ขาออกไปหา `api.telegram.org` ได้) ตอบเฉพาะ user (private chat) หรือ group ที่อยู่ใน allowlist เท่านั้น

**นี่คือ trust model ที่ต่างจาก `ask`/`coding` โดยสิ้นเชิง** — สองคำสั่งนั้นรันในเทอร์มินัลของผู้ดำเนินการเอง คนที่คุมพรอมต์คือคนที่คุมเครื่อง แต่ `telegrambot` รับข้อความจาก**คนอื่น**ผ่านอินเทอร์เน็ต ด้วยเหตุนี้ toolset ของ `telegrambot` จึงเล็กและเป็น read-only เท่านั้น:

| มีให้เสมอ | มีให้ถ้าตั้งค่าไว้ | **ไม่มีเลย** |
|---|---|---|
| `get_current_time`, `delay` | `search_knowledge`/`read_knowledge` (`--knowledge-dir`) | `read_file`/`write_file`/`edit_file`/`create_folder` |
| | `web_search`/`web_fetch` (เหมือน `ask`) | `run_command`, `scp_copy`, `api_request`, `ask_user` |

`search_knowledge`/`read_knowledge` sandbox อยู่ที่ไดเรกทอรีที่ตั้งค่าผ่าน `--knowledge-dir` เท่านั้น **ไม่ใช่** current directory ที่ตัว bot process รันอยู่ — คนละแนวคิดกับ `read_file`/`search_files` ของ `ask`/`coding` โดยเจตนา และไม่มี `write_knowledge`/`edit_knowledge` คู่กันเลย ฐานความรู้ต้องถูกดูแล/แก้ไขจากภายนอกเท่านั้น

**ค้นลึกเข้าไปใน subfolder ทุกชั้นเสมอ** — จัดเอกสารเป็นโฟลเดอร์ย่อยตามปี/ภาควิชา/หมวดหมู่ได้ตามสบาย (เช่น `2566/วิศวกรรมคอมพิวเตอร์/หลักสูตร.md`) `search_knowledge` จะเจอไฟล์เหล่านี้พร้อม path เต็มที่รวม subfolder ด้วยเสมอ ข้อยกเว้นเดียวคือ**โฟลเดอร์ที่ชื่อขึ้นต้นด้วย `.`** (เช่น `.git`, `.obsidian`) ซึ่งจะถูกข้ามเสมอตามธรรมเนียมไฟล์ซ่อนทั่วไป — โฟลเดอร์ชื่ออื่นๆ ไม่ว่าจะชื่ออะไรก็ค้นเจอหมด (ก่อนหน้านี้เคยใช้ denylist ของ `ask`/`coding` ที่ข้ามชื่อโฟลเดอร์อย่าง `build`/`bin`/`dist`/`vendor`/`target` แบบเงียบๆ ซึ่งเหมาะกับ source code แต่ไม่เหมาะกับโฟลเดอร์เอกสาร เพราะชื่อพวกนี้อาจเป็นชื่อโฟลเดอร์เอกสารจริงของคุณก็ได้ — แก้แล้วให้ไม่ใช้ denylist นั้นกับฐานความรู้อีกต่อไป)

**ถ้าตั้ง `--knowledge-dir` ไว้ system prompt จะสั่งให้โมเดิลลอง `search_knowledge` ก่อนเสมอสำหรับทุกคำถามที่ถาม** (ไม่ใช่แค่คำถามที่ดูเหมือนต้องใช้ข้อมูลเฉพาะ) ก่อนจะตอบจากความรู้ทั่วไปของตัวเอง แม้คำถามจะดูเป็นความรู้ทั่วไปที่ตอบได้อยู่แล้วก็ตาม — ยกเว้นข้อความที่ไม่ใช่คำถามจริงๆ (ทักทาย/พูดคุยเล่นๆ) ที่ไม่บังคับให้ค้น เหตุผลคือฐานความรู้อาจมีคำนิยาม/คำตอบเฉพาะที่ผู้ดูแลตั้งใจให้ใช้แทนความรู้ทั่วไปของโมเดิล (เช่น นิยามศัพท์เฉพาะวิชาที่ต่างจากที่โมเดิลรู้มา) **ข้อแลกเปลี่ยน:** ทุกคำถามจะมี tool call เพิ่มอย่างน้อย 1 รอบก่อนตอบเสมอ (latency เพิ่มขึ้นเล็กน้อยทุกครั้ง) ถ้าไม่ต้องการพฤติกรรมนี้ ให้ไม่ตั้ง `--knowledge-dir` เลย (ปิดทั้ง tool ไปพร้อมกัน)

### เริ่มต้นใช้งาน

```bash
export OLA_TELEGRAM_TOKEN='123456:ABC-your-bot-token-from-botfather'   # env เท่านั้น ไม่มี flag รับ token ตรงๆ
export OLA_OLLAMA_MODEL=qwen3.6:27b

# ยังไม่รู้ user id ของตัวเอง? รันแบบ allow ชั่วคราวด้วย id อะไรก็ได้ แล้วทัก /whoami กับบอทเพื่อดู id จริง
ola telegrambot --telegram-allowed-users 000000000

# ใช้งานจริง
ola telegrambot \
  --telegram-allowed-users 111111111,222222222 \
  --telegram-allowed-groups -1001234567890 \
  --persona 'คุณคือผู้ช่วยตอบคำถามวิชา Network Security ของภาควิชา ตอบสุภาพ กระชับ เป็นภาษาไทย' \
  --knowledge-dir /srv/course-docs/network-security \
  -x mytopic
```

ทัก `/whoami` (หรือ `/start`) กับบอทเมื่อไหร่ก็ได้ (แม้ยังไม่อยู่ใน allowlist) เพื่อดู user id / group chat id ของตัวเอง — เอาไปใส่ `--telegram-allowed-users`/`--telegram-allowed-groups` ได้เลย

ทัก `/tools` (เฉพาะแชทที่อยู่ใน allowlist แล้ว) เพื่อดูสถานะ tool แบบสดๆ จากในแชทเลย — บอกว่า `search_knowledge`/`web_search`/`web_fetch` แต่ละตัวเปิดอยู่ไหม, ฐานความรู้ชี้ไปที่ path ไหนจริงๆ (มีประโยชน์มากตอน debug ปัญหาแบบ "ค้นฐานความรู้/เว็บไม่ได้" เพราะ `telegrambot` มักรันเป็น background daemon ที่ log บนเทอร์มินัลอาจหลุดสายตาไปง่ายๆ)

**สาเหตุที่พบบ่อยที่สุดที่ทำให้ "ค้นหาไม่ได้":**
1. **`web_search` ต้องตั้ง backend เองเสมอ** (`--searxng-url` หรือ `--ollama-search-key`) — ถ้าไม่ตั้ง `web_search` จะถูกปิดโดยตั้งใจ (เหมือน `ola ask` ทุกประการ) `web_fetch` เปิด default ก็จริง แต่ทำได้แค่ "ดึงหน้าเว็บที่รู้ URL อยู่แล้ว" ไม่ใช่ "ค้นหา" แบบเปิดกว้าง — คำถามอย่าง "ราคาทองวันนี้เท่าไหร่" ต้องมี `web_search` เท่านั้นถึงจะตอบได้จริง
2. **`--knowledge-dir` เป็น relative path** — `telegrambot` รันเป็น daemon อายุยืน ถ้า start ผ่าน `nohup`/systemd/script wrapper ที่ cwd ไม่ใช่ที่คิดไว้ path แบบ `km` (ไม่ใช่ `/absolute/path/km`) อาจ resolve ไปคนละที่แล้วหา directory ไม่เจอ (จะมี warning ออกทาง stderr และใน log ไฟล์ตอนเริ่มทำงาน แต่พลาดดูง่ายมากถ้ารันแบบ background) **แนะนำใช้ absolute path เสมอสำหรับ `--knowledge-dir`/`--persona-file`/`--context-dir`** เพื่อตัดปัญหานี้ทิ้งไปเลย แล้วเช็คด้วย `/tools` ว่า path ที่ resolve ได้จริงตรงกับที่ตั้งใจไหม
3. **โมเดิลไม่ยอมเรียก tool เอง** — โดยเฉพาะกับ persona แบบ role-play/เพื่อนคุยเล่น โมเดิลอาจตอบจากความจำตัวเองทันทีโดยไม่ลองค้นก่อน ทั้งที่ system prompt สั่งให้ลอง search_knowledge ก่อนทุกคำถามอยู่แล้ว (ดูหัวข้อด้านบน) ถ้าเจอปัญหานี้บ่อย ลองเพิ่มประโยคย้ำใน `--persona`/`--persona-file` เองอีกชั้น หรือพิจารณาเปลี่ยนโมเดิลที่ tool-calling/instruction-following แข็งแรงกว่า

### พฤติกรรมใน group chat

บอทจะ**ไม่ตอบทุกข้อความ**ในกลุ่ม (จะกวนคนอื่นที่คุยเรื่องอื่นอยู่) — ตอบเฉพาะเมื่อ:
- ข้อความ mention บอท (`@botusername`)
- ข้อความเป็นการ reply ต่อข้อความของบอทเอง
- ข้อความขึ้นต้นด้วย `/ola` หรือ `/ask`

**แต่บอทจะบันทึกทุกข้อความในกลุ่มลง context เสมอ แม้ข้อความนั้นจะไม่ได้ mention ถึงบอทก็ตาม** — เพื่อให้พอถูก mention เมื่อไหร่ บอทจะเข้าใจบริบทของบทสนทนาก่อนหน้าในกลุ่มด้วย (ไม่ใช่เห็นแค่ข้อความที่ mention มันตรงๆ) แต่ละข้อความที่บันทึกจะระบุชื่อผู้พูดกำกับไว้ด้วยเสมอ (ใช้ first name ของ Telegram หรือ username ของ Discord) เพื่อให้บอทแยกออกว่าใครพูดอะไรไว้บ้างเมื่อมีหลายคนคุยกัน — ข้อความที่บันทึกแต่ไม่ตอบจะขึ้น log เป็น `[telegram_record]`/`[discord_record]` (แยกจาก `[assistant]` ที่เป็นรอบที่ตอบจริง) ให้ตรวจสอบได้ว่าอะไรถูกบันทึกไปบ้าง

ข้อควรรู้: กลุ่มที่ไม่อยู่ใน allowlist จะไม่ถูกบันทึกอะไรเลย (ไม่ว่าจะ mention บอทหรือไม่) — บอทไม่มีเหตุผลต้องจดจำบทสนทนาของกลุ่มที่ไม่ได้รับอนุญาตให้ใช้งานตั้งแต่แรก และ private chat (DM) ไม่มีการระบุชื่อผู้พูดกำกับ (มีคนเดียวคุยด้วยอยู่แล้ว การระบุชื่อไม่ได้เพิ่มข้อมูลอะไร)

### รองรับรูปภาพ — `--max-image-size`/`OLA_MAX_IMAGE_SIZE`

ทั้งสี่บอทแชท (`telegrambot`/`discordbot`/`linebot`/`webbot`) รับรูปภาพจากผู้ใช้ได้ ผ่าน `chatBotCore` ตัวเดียวกัน — หลักการเดียวกันหมดทุกบอท:

- **ต้องใช้โมเดิลที่รองรับ vision จริง** (เช่น `llava`, `qwen2-vl`, `gemma3`/`gemma4` บางเวอร์ชัน หรือ GPT-4V-style ผ่าน OpenAI-compatible) โมเดิลข้อความล้วนจะเห็นแค่ข้อความ ไม่เห็นรูป — system prompt สั่งให้โมเดิลบอกตรงๆ ว่าดูรูปไม่ได้แทนที่จะแต่งคำอธิบายขึ้นเอง แต่ก็ยังขึ้นกับว่าโมเดิลจะทำตามจริงแค่ไหน
- **`--max-image-size <size>`/`OLA_MAX_IMAGE_SIZE`** — ขนาดรูปภาพสูงสุดที่รับได้ เช่น `10K`, `12M`, `1G` (ไม่ใส่หน่วย = bytes) หน่วยเป็นแบบ binary (K=1024 ไม่ใช่ 1000) default **10M** — รูปเกินขนาดจะถูกปฏิเสธก่อนส่งให้โมเดิลเสมอ (Telegram/Discord ปฏิเสธได้ตั้งแต่ก่อนดาวน์โหลดด้วยซ้ำถ้าแพลตฟอร์มรายงานขนาดไฟล์มาให้ล่วงหน้า)
- **โดยปกติรูปภาพไม่ถูกบันทึกลง context ถาวร** (ทั้งที่เก็บลงดิสก์อย่าง telegrambot/discordbot/linebot และที่เก็บในหน่วยความจำอย่าง webbot) — โมเดิลเห็นรูปแค่ในเทิร์นที่ส่งมาเท่านั้น เทิร์นที่บันทึกไว้จะมีแค่ข้อความ + tag สั้นๆ เช่น `[แนบรูปภาพ 1 รูป]` แทน กันไม่ให้ context โตเร็วเกินไปจากไฟล์ base64 — **ถ้าอยากให้ดูรูปเก่าซ้ำข้ามบทสนทนาได้ ดูหัวข้อ `--save-images` ด้านล่าง**
- **ในกลุ่ม/channel: ดาวน์โหลดรูปเฉพาะข้อความที่ถูก mention เท่านั้น** (เหมือนหลักการ record-แต่ไม่ตอบ ของข้อความทั่วไป — ดูหัวข้อ [พฤติกรรมใน group chat](#พฤติกรรมใน-group-chat) ด้านล่าง) ข้อความที่มีรูปแต่ไม่ถูก mention จะถูกบันทึกเป็น `[ส่งรูปภาพ]` เฉยๆ ไม่ดาวน์โหลดจริง ประหยัด bandwidth

**ความต่างเฉพาะแพลตฟอร์ม:**
- **Telegram** — ดึงรูปจาก `photo` field (`getFile` แล้วดาวน์โหลดแยกอีกที) ข้อความประกอบรูปอยู่ใน `caption` ไม่ใช่ `text`
- **Discord** — attachment URL มาพร้อมข้อความอยู่แล้ว ดาวน์โหลดตรงได้เลยไม่ต้องมี lookup แยก
- **LINE** — ดาวน์โหลดผ่าน endpoint คนละ host กับ REST API ปกติ (`api-data.line.me`) และ**ข้อความรูปภาพของ LINE ไม่มี mention object เลย** จึงไม่มีทางที่รูปในกลุ่มจะ "ถูก mention" ได้ตามกลไกเดียวกับข้อความตัวอักษร — รูปที่ส่งในกลุ่ม LINE จะถูกบันทึกเป็น `[ส่งรูปภาพ]` เสมอ ไม่ถูกวิเคราะห์เลย (ส่งรูปใน DM ยังทำงานปกติ เพราะ DM ถือว่า addressed เสมออยู่แล้ว)
- **webbot** — มีปุ่มแนบรูป (📎) ข้าง input box พร้อม preview รูปก่อนส่งและปุ่มเอาออก

### บันทึกรูปภาพไว้อ้างอิงข้ามบทสนทนา — `--save-images`/`OLA_SAVE_IMAGES`

**เฉพาะ telegrambot/discordbot/linebot เท่านั้น — ไม่มีใน webbot โดยเจตนา** เพราะขัดกับดีไซน์ "ไม่ persist อะไรเลย" ของ webbot ที่ตั้งใจไว้ตั้งแต่แรก

ปิดโดย default (`--save-images`/`OLA_SAVE_IMAGES=1`/`true`/`yes`/`on` เพื่อเปิด) — เป็น **opt-in ไม่ใช่พฤติกรรมเริ่มต้นใหม่** เพราะเก็บรูปจริงที่ผู้ใช้ส่งมาไว้ถาวรมีนัยเรื่อง privacy/พื้นที่ดิสก์ที่มากกว่าการเก็บแค่ text tag เดิม

**เปิดแล้วเกิดอะไรขึ้น:**
- **ดาวน์โหลด+บันทึกรูปแม้ข้อความจะไม่ถูก mention ก็ตาม** (ต่างจาก default เดิมที่ไม่มี `--save-images` ซึ่งดาวน์โหลดเฉพาะข้อความที่ addressed เท่านั้น) — เพื่อให้ดึงรูปที่ใครโพสต์ทิ้งไว้ในห้องมาถามทีหลังได้ แม้ตอนโพสต์จะไม่ได้ mention บอทเลย **แต่รูปที่ไม่ addressed จะไม่ถูกส่งให้โมเดิลวิเคราะห์ทันที** (ไม่มีการเรียกโมเดิลอยู่แล้วตอนไม่ addressed) แค่บันทึกไว้เฉยๆ รอให้โมเดิลเรียก `read_pic` เองทีหลังตอนถูกถามถึงจริง — สำหรับ **LINE เรื่องนี้สำคัญเป็นพิเศษ** เพราะข้อความรูปภาพของ LINE ไม่มีทาง "ถูก mention" ได้เลยตามที่อธิบายไว้ด้านบน (ไม่มี mention object) ถ้าไม่เปิด `--save-images` รูปที่ส่งในกลุ่ม LINE จะไม่มีทางถูกอ้างอิงถึงได้อีกเลยไม่ว่ากรณีใด
- รูปที่ดาวน์โหลดมาถูกบันทึกไว้ที่ `<context-dir>/pics/<chat-key>/<timestamp>_<hash>.<นามสกุล>` — **แยกโฟลเดอร์ตามแชทเสมอ** ไม่ใช่โฟลเดอร์รวม กันรูปจากแชทหนึ่งถูกแชทอื่นมองเห็น/อ้างอิงถึงได้โดยไม่ตั้งใจ (เหมือนกับที่ context ไฟล์ของแต่ละแชทก็แยกกันอยู่แล้ว)
- เทิร์นที่บันทึกไว้จะมี tag ระบุ path จริงแทนตัวเลขนับรูปเฉยๆ เช่น `[แนบรูปภาพ: pics/group_-999/20260814-143022_a1b2c3d4.jpg]` — ไม่ว่าจะ addressed หรือไม่ก็ตาม
- โมเดิลมี tool ใหม่ **`read_pic`** ให้เรียกดูรูปเก่าอีกครั้งเมื่อผู้ใช้อ้างถึง (เช่น "รูปที่ส่งไปเมื่อวันก่อน...") — รับ path ตรงตามที่ปรากฏใน context เดิม แล้วแนบรูปกลับเข้า tool loop เป็นข้อความใหม่ (กลไกเดียวกับที่ `ask`/`coding`'s `read_pdf` ใช้แนบหน้า PDF ที่แปลงเป็นรูปกลับเข้าบทสนทนา) sandbox เฉพาะ `pics/` เท่านั้น เข้าไฟล์อื่นใน context-dir (เช่น `knowledge-index.json`) ไม่ได้
- **ข้อควรรู้เรื่องความแม่นยำ:** การอธิบายรูปที่เรียกผ่าน `read_pic` อาจแม่นยำน้อยกว่าตอนแนบรูปให้บอทดูตรงๆ ในเทิร์นเดียวกัน เพราะมี tool call/tool result คั่นกลางระหว่างคำถามจริงกับรูปที่แนบมา ทำให้โมเดิล (โดยเฉพาะโมเดิลขนาดเล็กหรือบางเวอร์ชันที่ต้องพึ่ง cloud routing) มีโอกาส "หลุดโฟกัส" ได้ง่ายกว่า — ระบบมีข้อความกำกับชัดเจนที่ท้ายเทิร์น follow-up ย้ำให้โมเดิลตอบตามสิ่งที่เห็นจริงในรูปเท่านั้น ห้ามเดา, compaction ถูกสั่งให้เก็บ path ของรูปไว้แบบคำต่อคำเสมอ (ไม่ถอดความทิ้ง), และมีกฎบังคับให้โมเดิลต้องเรียก `read_pic` ก่อนตอบเสมอเมื่อคำถามพูดถึงรูปที่ไม่ได้แนบมาในข้อความปัจจุบัน (กันกรณีโมเดิลตอบเดา/มั่วไปเลยโดยไม่เรียก tool ก่อน) แต่สุดท้ายความแม่นยำยังขึ้นกับว่าโมเดิลที่ใช้ทำตามคำสั่งได้ดีแค่ไหนด้วย

**ข้อควรรู้ที่สำคัญ:**
- ไม่มีการลบรูปเก่าอัตโนมัติเลย — `pics/` จะโตขึ้นเรื่อยๆ ตามจำนวนรูปที่ส่งเข้ามา **ผู้ดูแลต้องจัดการพื้นที่ดิสก์เอง** (เช่น cron job ลบไฟล์เก่ากว่า N วัน) เหมือนกับที่ `knowledge-index.json` ก็ไม่มีการ prune อัตโนมัติเช่นกัน
- รูปที่ save ไว้ไม่ถูกส่งกลับเข้า context ทุกรอบอัตโนมัติ (ถ้าทำแบบนั้นจะแพงขึ้นเรื่อยๆ ตามจำนวนรูปสะสม) โมเดิลต้องเรียก `read_pic` เองเมื่อต้องการดูจริงๆ เท่านั้น
- ข้อความที่ไม่ addressed จะไม่มีการตอบกลับใดๆ เสมอ (ทั้งตอนบันทึกสำเร็จและตอนล้มเหลว เช่น รูปเกิน `--max-image-size` หรือดาวน์โหลดไม่สำเร็จ) ตรงกับหลักการเดิมที่ข้อความไม่ addressed จะถูก "บันทึกเงียบๆ" เสมอ ไม่ส่งอะไรกลับเลย

### Persona — เติมต่อท้ายเท่านั้น ไม่ใช่ override

หลักการ "system prompt คงที่ ไม่มี `-s/--system`" (ดู [ภาพรวมและปรัชญาการออกแบบ](#ภาพรวมและปรัชญาการออกแบบ)) ยังใช้กับ `telegrambot` เช่นกัน — `--persona`/`--persona-file` ถูกแทรกเข้าไป**ระหว่าง**ประโยคเปิดกับกติกาพื้นฐานของ system prompt ที่ตายตัว (ไม่ใช่ต่อท้ายทั้งหมด) เพื่อให้ชื่อ/บุคลิกที่กำหนดไว้มีน้ำหนักสูงสุดในสายตาโมเดิล — พบจากการทดสอบจริงว่าถ้า persona ถูกแปะไว้ท้ายสุด (หลังกติกายาวๆ) โมเดิลบางตัวมีแนวโน้มตอบชื่อแบบทั่วไป เช่น "ฉันชื่อบอท" แทนชื่อที่ตั้งไว้ กติกาความปลอดภัย/ขอบเขต tool ที่มีให้ ไม่สามารถถูกเปลี่ยนผ่าน persona ได้ไม่ว่าจะวางตำแหน่งไหน

**ถ้ายังเจอปัญหาโมเดิลไม่ยอมใช้ชื่อ/บุคลิกตาม persona แม้ปรับตำแหน่งแล้ว** นี่มักเป็นข้อจำกัดของตัวโมเดิลเอง (พบบ่อยกับโมเดิลขนาดเล็ก/quantized ที่ instruction-following ไม่แข็งแรงพอ โดยเฉพาะกับคำถามสั้นๆ แบบ "คุณชื่ออะไร") ลองเสริมประโยคย้ำใน persona ให้ชัดขึ้นอีก หรือพิจารณาเปลี่ยนไปใช้โมเดิลที่ใหญ่ขึ้น/instruction-tuned ดีกว่า

### ความจำต่อแชท (persistent, auto-compact)

แต่ละ private chat / group แยกไฟล์ context ของตัวเองไว้ที่ `--context-dir` (default: `telegram-context/`) เป็น JSON หนึ่งไฟล์ต่อหนึ่งแชท (`user_<id>.json` / `group_<id>.json`) เขียนแบบ atomic (`.tmp` แล้ว rename) ป้องกันไฟล์เสียหายถ้า process โดน kill กลางคัน

เมื่อจำนวน turn ในแชทหนึ่งเกิน `--context-compact-after` (default 300) ola จะเรียกโมเดิลสรุปบทสนทนาที่เก่ากว่า `--context-keep-recent` (default 100) turn ล่าสุด เก็บเป็น "สรุปบทสนทนาก่อนหน้า" แทนที่ turn ดิบทั้งหมด — **นี่คือการสรุปเนื้อหาจริงด้วยโมเดล ไม่ใช่แค่ label เหมือนที่ `ola coding` ทำกับ context ของตัวเอง**เพราะบทสนทนาไม่มี state อื่นให้ recover เนื้อหาคืนได้แบบ `PROGRESS.md`/`read_file` ของ `ola coding`

### สิ่งที่ยังไม่รองรับ (รู้ไว้ก่อน)

- `read_knowledge` ยังไม่รองรับไฟล์ `.pdf` (ต่างจาก `read_file` ที่มี `read_pdf` ช่วย)
- ไม่มี `ask_user` แบบ `ola ask` — ถ้าโมเดิลพยายามถามกลับ จะได้ error กลับไปเหมือนรันแบบ non-interactive (โมเดิลต้องตัดสินใจเองหรือถามในคำตอบปกติแทน เพราะทุกข้อความในแชทคือเทิร์นของบทสนทนาต่อเนื่องอยู่แล้ว)
- ไม่มี per-user rate limit ละเอียด — มีแค่ mutex ต่อแชท (กันสองข้อความจากแชทเดียวกันแข่งกันเขียน context) และ `--telegram-max-concurrent` (จำกัดจำนวนข้อความที่ประมวลผลพร้อมกันทั้งโปรเซส)

### เรื่อง `--poll-timeout` ค่าสูงๆ (default 600 วินาที)

`getUpdates` ของ Telegram ไม่มีเอกสารระบุค่าสูงสุดของ `timeout` อย่างเป็นทางการ (แค่บอกว่า "ควรเป็นค่าบวก") ในทางปฏิบัติมีรายงานผลไม่ตรงกัน — บางคนใช้ 300 วินาทีได้ปกติ บางคนเจอ connection ถูกตัดกลับมาเร็วกว่าที่ตั้งไว้มาก (เช่น ที่ ~50 วินาที) ซึ่งน่าจะมาจากตัวกลาง (proxy/load balancer) ระหว่างทางมากกว่าตัว Telegram API เอง ไม่ใช่ข้อจำกัดที่ตายตัว — ผลข้างเคียงถ้า connection ถูกตัดกลางคันไม่ใช่ปัญหาร้ายแรงอะไร (`buildTelegramHTTPClients` ตั้ง client-side timeout ไว้ที่ `--poll-timeout + 15` วินาทีเสมอ พอ read ล้มเหลวหรือ timeout ก็แค่วนลอง `getUpdates` ใหม่รอบถัดไปตามปกติ) แต่ถ้าสังเกตว่าบอทตอบช้ากว่าที่ควรหรือ log มี error จาก `getUpdates` บ่อยผิดปกติ ลองลดค่านี้ลงมาที่ 60-300 วินาทีดูก่อน

### ถ้าเจอ "context deadline exceeded" ตลอด

ถ้า log ขึ้น `Post "http://127.0.0.1:11434/api/chat": context deadline exceeded (Client.Timeout exceeded while awaiting headers)` ทุกข้อความแม้แต่ทักทายธรรมดา — เวอร์ชันก่อนหน้ามีบั๊กที่ client เรียก Ollama/OpenAI ใช้ timeout สั้นแบบเดียวกับที่เรียก Telegram API (30 วินาที) ทำให้โมเดิลขนาดใหญ่ (เช่น 35B) ที่ตอบช้ากว่า 30 วินาทีบนเครื่องที่ไม่แรงพอ **ล้มเหลวทุกครั้ง** ไม่ว่าจะตั้ง `--poll-timeout` เท่าไหร่ก็ตาม (`--poll-timeout` มีผลแค่กับ `getUpdates` เท่านั้น ไม่เกี่ยวกับการเรียกโมเดิล) — แก้แล้วในเวอร์ชันนี้ด้วยการแยก HTTP client ของการเรียกโมเดิลออกจาก Telegram API โดยเด็ดขาด (ไม่มี timeout เลย เหมือนที่ `ask`/`coding` ใช้ ตรงกับที่โมเดิลใหญ่อาจต้องใช้เวลานานในการตอบจริง)

### ถ้า `search_knowledge` หาไม่เจอทั้งที่คำตอบอยู่ในเอกสารจริง

`search_knowledge` ที่มี `query` ทำงานแบบ **grep บรรทัดที่ตรงกับ query แบบตรงตัว (exact substring)** ไม่ใช่การค้นแบบเข้าใจความหมาย — ถ้าคำถามใช้คำว่า "สัตว์เลี้ยง" แต่เอกสารเขียนว่า "หมา"/"แมว" การ grep แบบตรงตัวจะหาไม่เจอแม้เอกสารมีคำตอบอยู่จริงๆ เพราะไม่มีบรรทัดไหนมีคำว่า "สัตว์เลี้ยง" ตรงๆ เลย

เพื่อลดปัญหานี้ ถ้า `pattern` เจอไฟล์แต่ `query` ไม่เจอบรรทัดที่ตรงเลย และไฟล์ที่เจอมีไม่เกิน 5 ไฟล์ ระบบจะ**แนบเนื้อหาไฟล์เหล่านั้นมาให้โมเดิลอ่านเองโดยตรง** (แทนที่จะตอบแค่ "ไม่พบ") ให้โมเดิลหาคำตอบจากเนื้อหาจริงแทนการพึ่ง grep เพียงอย่างเดียว ถ้าไฟล์ที่เจอมีมากกว่า 5 ไฟล์ จะแสดงรายชื่อไฟล์ที่ pattern ตรงแทน เพื่อให้โมเดิลเลือกอ่านไฟล์ที่เกี่ยวข้องด้วย `read_knowledge` ต่อได้เอง

ถ้าฐานความรู้มีไฟล์จำนวนมาก แนะนำให้โมเดิลลองด้วย `pattern: "*"` แล้วไม่ใส่ `query` (จะได้รายชื่อไฟล์ทั้งหมด) ก่อน แล้วค่อยเลือกอ่านไฟล์ที่เกี่ยวข้องด้วย `read_knowledge` ตรงๆ ถ้ารู้สึกว่าการ grep ด้วย query ไม่ช่วยอะไร

**ทางแก้ที่ตรงจุดกว่า:** เปิด embedding-based (semantic) search ด้วย `--embed-model` — ดูหัวข้อถัดไป

### Embedding-based (semantic) search — `--embed-model`

เมื่อ grep แบบตรงตัวหาไม่เจอ (แต่ `pattern` เจอไฟล์อยู่) และตั้ง `--embed-model` ไว้ `search_knowledge` จะ**ลองค้นแบบเข้าใจความหมายต่อโดยอัตโนมัติ** ก่อนจะ fallback ไปแนบเนื้อหาไฟล์แบบข้อก่อนหน้า — แก้ปัญหา "สัตว์เลี้ยง" vs "หมา/แมว" ได้ตรงจุดกว่าการ dump ทั้งไฟล์ โดยเฉพาะเมื่อฐานความรู้มีไฟล์เยอะขึ้นจนไฟล์ที่ match pattern เกิน 5 ไฟล์บ่อยๆ (fallback แบบเดิมจะเปลี่ยนไปแสดงแค่รายชื่อไฟล์แทนเนื้อหา)

**สถาปัตยกรรมโดยย่อ:** ไม่มี vector database ภายนอก — ใช้ brute-force cosine similarity ในหน่วยความจำ (เร็วพอสำหรับฐานความรู้ขนาดหลักร้อยไฟล์) เรียก Ollama's native `/api/embed` โดยตรงผ่าน `net/http` (ไม่มี Go dependency ใหม่) แบ่งแต่ละไฟล์เป็น chunk ก่อน embed (ตัดตาม paragraph, chunk ที่ยาวเกินไปตัดซ้ำแบบ sliding window พร้อม overlap — ตัดแบบ rune-safe ไม่มีทางตัดกลางตัวอักษรไทยหลาย byte) ระบบจะ:

1. **ตอนเริ่มทำงาน**: โหลด index เดิมจาก `<context-dir>/knowledge-index.json` (ถ้ามี) แล้ว **embed ทุกไฟล์ใหม่/ที่เปลี่ยนแปลงทันที** ก่อนเข้า loop รับข้อความ (ครั้งแรกที่ไม่มี index เดิม = embed ทั้งฐานความรู้ทันที)
2. **ระหว่างรัน**: walk ไฟล์ใหม่เป็นระยะ (`--embed-refresh-interval`, default ทุก 1 นาที) เปรียบเทียบ hash เนื้อหาไฟล์กับที่ index ไว้ — ไฟล์ที่ไม่เปลี่ยนใช้ embedding เดิมจาก cache ไฟล์ใหม่/เปลี่ยนแปลงค่อย embed ใหม่เฉพาะไฟล์นั้น ไฟล์ที่ถูกลบจะหายไปจาก index ในรอบถัดไปด้วย
3. **ตอนค้น**: ถ้า grep ไม่เจอเลย embed query (1 ครั้ง) แล้วเทียบ cosine similarity กับทุก chunk ที่อยู่ในไฟล์ที่ `pattern` match (จำกัดเฉพาะไฟล์ที่ pattern เลือกไว้ เพื่อให้ pattern ยังมีความหมาย) คืน top-`--embed-top-k` ที่คะแนนเกิน `--embed-min-score`

**ข้อจำกัดสำคัญ:**
- รองรับเฉพาะ `--provider ollama` ในตอนนี้ (เรียก `/api/embed` ของ Ollama โดยตรง — ยังไม่รองรับ OpenAI-compatible embeddings endpoint)
- ต้อง `ollama pull` โมเดิล embedding แยกต่างหากจากโมเดิลแชท — **สำหรับฐานความรู้ภาษาไทย ควรเลือกโมเดิล embedding ที่รองรับหลายภาษา** (เช่น `bge-m3`) โมเดิลขนาดเล็กที่เทรนภาษาอังกฤษเป็นหลัก (เช่น `all-minilm`) มักให้ผลแย่กับข้อความไทยล้วน
- `--embed-min-score` (default 0.35) เป็นแค่**ค่าตั้งต้น ไม่ใช่ค่าที่ผ่านการทดสอบจริง** — คะแนน cosine similarity ที่เหมาะสมขึ้นกับโมเดิล embedding ที่ใช้จริง ควรทดลองปรับตามผลลัพธ์ที่เห็นจริง
- ยังเป็น fallback ชั้นที่สองอยู่ดี ไม่ใช่ตัวแทน grep แบบตรงตัว — grep ที่เจอ hit ตรงๆ ยังชนะเสมอ (เร็วกว่า ไม่ต้องเรียก API เพิ่ม)

เช็คสถานะ embedding index (จำนวน chunk, โมเดิลที่ใช้, ตั้งค่าอะไรไว้) ได้จากคำสั่ง `/tools` ในแชท หรือดู log ตอน bot เริ่มทำงาน

### ถ้าโมเดิลตอบว่างเปล่า

บางครั้งโมเดิล (โดยเฉพาะ reasoning model ที่ใช้ token งบประมาณไปกับ "thinking" จนหมดก่อนจะสร้างคำตอบจริง) อาจตอบกลับมาแบบไม่มีทั้งข้อความและไม่มี tool call เลย — `telegrambot` เจอกรณีนี้จะ**ลองใหม่ให้อัตโนมัติ 1 ครั้ง** (ส่งข้อความเตือนกลับไปให้โมเดิลว่าคำตอบก่อนหน้าว่างเปล่า) ถ้ายังว่างอีกจะถือเป็น error — ผู้ใช้จะได้ข้อความแจ้งปัญหากลับไปแทนที่จะได้ข้อความว่างเปล่า/ไม่มีอะไรตอบเลย และ **จะไม่มีการบันทึก turn ที่ล้มเหลวลง context ไฟล์** เช็ค log (`-o`) จะเห็นบรรทัด `[telegram_warning] โมเดลตอบว่างเปล่า` ถ้าเจอปัญหานี้บ่อย มักเป็นสัญญาณว่าโมเดิลที่ใช้อยู่เล็ก/ตั้ง `-c/--ctx` ต่ำเกินไปสำหรับ persona+ประวัติสนทนาที่ยาวขึ้นเรื่อยๆ

### ถ้าโมเดิล "แต่ง" ผลค้นหาขึ้นเองแทนที่จะค้นจริง

โมเดิลบางตัว **แม้จะเรียก tool อื่นได้ถูกต้องจริง** (เช่น `get_current_time`) แต่บางครั้งกลับเลือก "เล่าเรื่อง" คำตอบให้ดูเหมือนเพิ่งค้นเว็บมา (มี 🔍, "ผลการค้นหา", "คำค้นที่ 1/2/3") ทั้งที่ไม่ได้เรียก `web_search`/`web_fetch` จริงเลย — สังเกตได้ง่ายเพราะผลลัพธ์จริงจาก `web_search`/`web_fetch` จะมี URL จริงแนบมาด้วยเสมอ (ดูรูปแบบใน [Web search / web fetch](#web-search--web-fetch)) ถ้าคำตอบอ้างว่าค้นแล้วแต่ไม่มี URL อ้างอิงเลย หรือถามซ้ำเรื่องเดิมในเวลาใกล้กันแล้วได้ตัวเลข/วันที่ไม่ตรงกัน นั่นคือสัญญาณว่าโมเดิลกำลังมโนคำตอบ ไม่ใช่ค้นจริง

เพื่อลดปัญหานี้ ทุกผลลัพธ์จริงจาก `web_search`/`web_fetch`/`search_knowledge`/`read_knowledge` ที่ส่งกลับให้โมเดิลจะถูกครอบด้วย marker `[ผลลัพธ์จริงจากการเรียก ... เมื่อครู่นี้]` เสมอ คู่กับกติกาใน system prompt ที่ห้ามเขียนคำตอบแบบ "ผลการค้นหา" นอกเหนือจากที่มี marker นี้กำกับอยู่จริง — **เป็นการลดโอกาสเกิด ไม่ใช่การรับประกัน** ถ้าโมเดิลไม่ทำตาม instruction อยู่แล้วเรื่องอื่นๆ ก็มีโอกาสไม่ทำตามกติกานี้ด้วยเช่นกัน วิธีตรวจสอบที่ชัดเจนที่สุดคือเปิด log (`-o`) แล้วหาว่ามีบรรทัด `[tool_call] web_search(...)` ตรงกับช่วงเวลาของคำถามนั้นจริงไหม — ถ้าไม่มีเลย แปลว่าโมเดิลไม่ได้เรียก tool จริง ให้พิจารณาเปลี่ยนโมเดิลที่ instruction-following/tool-calling แข็งแรงกว่า

### Flags / Environment variables

| ตัวแปร | Flag | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `OLA_TELEGRAM_TOKEN` | *(env เท่านั้น)* | — | **จำเป็น** — bot token จาก [@BotFather](https://t.me/BotFather) ไม่มี flag รับตรงๆ เพื่อไม่ให้หลุดไปอยู่ใน shell history/`ps` |
| `OLA_TELEGRAM_ALLOWED_USERS` | `--telegram-allowed-users` | — | comma-separated Telegram user ID (ตัวเลขเท่านั้น ไม่รับ `@username` เพราะเปลี่ยนได้) |
| `OLA_TELEGRAM_ALLOWED_GROUPS` | `--telegram-allowed-groups` | — | comma-separated Telegram group/supergroup chat ID |
| `OLA_PERSONA` | `--persona` | — | ข้อความ persona/คำสั่งเพิ่มเติม เติมต่อท้าย system prompt (ใช้ร่วมกับ `ola discordbot`) |
| `OLA_PERSONA_FILE` | `--persona-file` | — | ไฟล์ persona (ชนะ `--persona`/`OLA_PERSONA` ถ้าตั้งทั้งคู่ - ใช้ร่วมกับ `ola discordbot`) |
| `OLA_KNOWLEDGE_DIR` | `--knowledge-dir` | — | comma-separated directory สำหรับ `search_knowledge`/`read_knowledge` (ใช้ร่วมกับ `ola discordbot`) |
| `OLA_EMBED_MODEL` | `--embed-model` | — | Ollama embedding model (เช่น `bge-m3`) เปิด semantic search fallback (`--provider ollama` เท่านั้น, ใช้ร่วมกับ `ola discordbot`) |
| — | `--embed-top-k` | `5` | จำนวน chunk สูงสุดจาก semantic search |
| — | `--embed-min-score` | `0.35` | คะแนน cosine similarity ขั้นต่ำ (ค่าตั้งต้น ควรปรับตามโมเดิลจริง) |
| — | `--embed-refresh-interval` | `60` วินาที | ความถี่ walk ไฟล์หาการเปลี่ยนแปลง (index ที่ `<context-dir>/knowledge-index.json`) |
| `OLA_CONTEXT_DIR` | `--context-dir` | `telegram-context` | ที่เก็บไฟล์ context ราย user/group (ตัวแปรเดียวกับ `ola discordbot` — ค่า default ต่างกันถ้าไม่ได้ตั้ง แต่ตั้ง `OLA_CONTEXT_DIR` แล้วทั้งสองบอทจะไปที่เดียวกัน ปลอดภัยเพราะ key ไม่ชนกัน) |
| — | `--context-keep-recent` | `100` | จำนวน turn ล่าสุดที่เก็บแบบเต็มหลัง compact |
| — | `--context-compact-after` | `300` | compact เมื่อจำนวน turn เกินนี้ (ต้องมากกว่า `--context-keep-recent`) |
| `OLA_TELEGRAM_API_BASE` | `--telegram-api-base` | `https://api.telegram.org` | override สำหรับทดสอบกับ mock server |
| — | `--poll-timeout` | `600` | long-poll timeout วินาทีต่อ `getUpdates` (10 นาที — ดูหมายเหตุด้านล่างเรื่องขีดจำกัดฝั่ง Telegram) |
| — | `--telegram-max-concurrent` | `4` | จำนวนข้อความสูงสุดที่ประมวลผลพร้อมกันทั้งโปรเซส |
| `OLA_MAX_IMAGE_SIZE` | `--max-image-size` | `10M` | ขนาดรูปภาพสูงสุด เช่น `10K`/`12M`/`1G` — ดูหัวข้อ "รองรับรูปภาพ" ด้านล่าง |
| `OLA_SAVE_IMAGES` | `--save-images` | ปิด | บันทึกรูปที่ผู้ใช้ส่งไว้ที่ `<context-dir>/pics/` เพื่อให้โมเดิลเรียก `read_pic` ดูซ้ำได้ — ดูหัวข้อ "บันทึกรูปภาพไว้อ้างอิงข้ามบทสนทนา" ด้านล่าง |
| — | `-o, --output` | `telegrambot.log` | log ไฟล์แบบเต็ม เปิดแบบ **append เสมอ** (ต่างจาก `ask`/`coding` ที่ overwrite เป็น default — `telegrambot` เป็น process เดียวรันยาวข้าม restart) |

ตัวแปรที่เหลือ (การเชื่อมต่อโมเดล `-m/-c/-P/--api-base/-k`, `-x/--topic`, และ web search/fetch ทั้งชุด `--searxng-url`/`--ollama-search-key`/`--no-web-search`/ฯลฯ) ใช้ร่วมกับ `ola ask` ทุกประการ — ดู [ตัวแปรสภาพแวดล้อมทั้งหมด](#ตัวแปรสภาพแวดล้อม-environment-variables-ทั้งหมด) และ [Web search / web fetch](#web-search--web-fetch) `web_search` เปิดอัตโนมัติเมื่อมีการตั้ง backend ไว้ (SearXNG หรือ Ollama search key) ส่วน `web_fetch` เปิดอัตโนมัติเสมอเหมือน `ask` จนกว่าจะสั่ง `--no-web-search`

---

## `ola discordbot`

Discord counterpart ของ `ola telegrambot` — **หลักการ, toolset, persona, ฐานความรู้ (grep + embedding search), context compaction, grounding marker กัน hallucination ทั้งหมดเหมือนกันทุกประการ** เพราะทั้งสองบอทใช้ `chatBotCore` ตัวเดียวกัน (ดู [ภาพรวมและปรัชญาการออกแบบ](#ภาพรวมและปรัชญาการออกแบบ)) — **หัวข้อนี้จะพูดถึงเฉพาะส่วนที่ต่างจาก `telegrambot` เท่านั้น** ส่วนที่เหมือนกัน (persona ordering, embedding search, `/tools`-เทียบเท่า, ฯลฯ) ดูรายละเอียดเต็มได้ที่ [`ola telegrambot`](#ola-telegrambot)

### ความต่างสำคัญ: ไม่มี long-polling ต้องใช้ WebSocket Gateway

Discord ไม่มี endpoint แบบ `getUpdates` ของ Telegram — บอทต้องเปิด **WebSocket connection ค้างไว้ตลอดเวลา** ไปที่ Discord's Gateway เพื่อรับข้อความ ไม่มีทางเลี่ยงด้วย REST อย่างเดียว เนื่องจาก Go standard library ไม่มี WebSocket client เลย โปรเจกต์นี้เลยต้องมี **Go dependency ภายนอกตัวเดียว** คือ `github.com/gorilla/websocket` ใช้เฉพาะสำหรับ WebSocket frame transport เท่านั้น — ทุกอย่างที่เหนือกว่านั้น (Gateway protocol เอง: HELLO/IDENTIFY/HEARTBEAT/DISPATCH, REST client, rate-limit handling) ยังเขียนด้วย `net/http`/`encoding/json` ตรงๆ เหมือน integration อื่นทุกตัวในโปรเจกต์นี้

**ข้อจำกัดที่ตั้งใจไว้:** เมื่อ connection หลุด (ไม่ว่าจากเหตุผลอะไร) discordbot จะ **reconnect ด้วย IDENTIFY ใหม่เสมอ** แทนที่จะ implement RESUME handshake (OP 6 ของ Discord ที่ replay session เดิมเพื่อกู้ event ที่พลาดไปช่วงสั้นๆ ตอน reconnect) — IDENTIFY ใหม่เป็นพฤติกรรมที่ Discord รองรับอย่างเป็นทางการเช่นกัน (ไม่ใช่ workaround) เพียงแต่มีโอกาสพลาดข้อความที่ถูกส่งมาในช่วงไม่กี่วินาทีระหว่าง reconnect ถ้าเรื่องนี้สำคัญกับการใช้งานของคุณ อาจพิจารณา implement RESUME เพิ่มเติมในอนาคต

### ⚠️ ต้องเปิด "Message Content Intent" ด้วยมือใน Discord Developer Portal

การอ่านเนื้อหาข้อความ (`content`) เป็น **privileged intent** ของ Discord ต้องไปเปิดที่ Developer Portal → Bot → Privileged Gateway Intents ด้วยตัวเอง (โค้ดขอ intent นี้ตอน IDENTIFY อยู่แล้ว แต่ Discord จะไม่อนุญาตถ้ายังไม่ได้เปิดในพอร์ทัล) **ลืมเปิดจุดนี้คือสาเหตุอันดับหนึ่งที่บอท online ปกติแต่เพิกเฉยทุกข้อความเงียบๆ โดยไม่มี error ให้เห็นเลย** เช็คจุดนี้ก่อนเป็นอันดับแรกถ้าบอทดูเหมือนไม่ตอบอะไร

### เริ่มต้นใช้งาน

```bash
export OLA_DISCORD_TOKEN='your-bot-token-from-discord-developer-portal'
export OLA_OLLAMA_MODEL=qwen3.6:27b

# ยังไม่รู้ user id ของตัวเอง? ทัก !whoami กับบอทหลัง DM ได้ (ทำงานแม้ยังไม่อยู่ใน allowlist)
ola discordbot --discord-allowed-users 000000000000000000

# ใช้งานจริง
ola discordbot \
  --discord-allowed-users 111111111111111111 \
  --discord-allowed-guilds 222222222222222222 \
  --persona 'คุณคือผู้ช่วยตอบคำถามวิชา Network Security ของภาควิชา ตอบสุภาพ กระชับ เป็นภาษาไทย' \
  --knowledge-dir /srv/course-docs/network-security \
  --embed-model bge-m3
```

### Allowlist สามชั้น (ต่างจาก Telegram ที่มีสองชั้น)

Discord ซ้อนโครงสร้างลึกกว่า Telegram: Server (guild) > Channel > Message

| Flag | ความหมาย |
|---|---|
| `--discord-allowed-users` | user ID ที่ DM ได้ (เหมือน `--telegram-allowed-users`) |
| `--discord-allowed-guilds` | server ID ที่บอทตอบได้ |
| `--discord-allowed-channels` | **optional** — จำกัดเฉพาะบาง channel ใน guild ที่อนุญาต ถ้าไม่ตั้งจะตอบได้ทุก channel ของ guild นั้น |

### พฤติกรรมใน guild channel

ตอบเฉพาะเมื่อถูก **@mention** (เช็คจาก `mentions` array ที่ Discord parse มาให้แล้ว ไม่ได้ regex เอง) หรือขึ้นต้นด้วย `!ola`/`!ask` — **ยังไม่รองรับการตอบกลับด้วย reply** (ต่างจาก Telegram ที่ reply ข้อความบอทก็ทำงาน) เพราะ Discord's `message_reference` มีแค่ `message_id` ไม่มีข้อมูลผู้เขียนข้อความที่ถูก reply ติดมาด้วย ต้องมี REST call เพิ่มไปดึงข้อความนั้นก่อนถึงจะรู้ว่าเป็นข้อความของบอทหรือเปล่า — ตัดออกไปก่อนเพื่อไม่ต้องมี round-trip เพิ่มทุกข้อความในกลุ่ม

### ความจำต่อแชท: แยกตาม channel ไม่ใช่ทั้ง server

ต่างจาก Telegram ที่ group หนึ่งกลุ่ม = context หนึ่งไฟล์ Discord แยกความจำเป็น**รายแชนแนล** (`discord_channel_<id>.json`) เพราะแต่ละ channel ในเซิร์ฟเวอร์เดียวกันมักคุยกันคนละเรื่อง ให้แต่ละ channel มีความจำของตัวเองสมเหตุสมผลกว่าแชร์ความจำทั้งเซิร์ฟเวอร์ ส่วน DM ใช้ `discord_dm_<user id>.json` — คีย์ทั้งหมดขึ้นต้นด้วย `discord_` เสมอ ชนกับ key ของ Telegram (`user_<id>`/`group_<id>`) ไม่ได้ ดังนั้น**ใช้ `--context-dir` เดียวกันกับ telegrambot ได้อย่างปลอดภัย** ถ้าอยากรันสองบอทคู่กัน

### Flags / Environment variables

| ตัวแปร | Flag | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `OLA_DISCORD_TOKEN` | *(env เท่านั้น)* | — | **จำเป็น** — bot token จาก Discord Developer Portal |
| `OLA_DISCORD_ALLOWED_USERS` | `--discord-allowed-users` | — | comma-separated user ID (DM) |
| `OLA_DISCORD_ALLOWED_GUILDS` | `--discord-allowed-guilds` | — | comma-separated server ID |
| `OLA_DISCORD_ALLOWED_CHANNELS` | `--discord-allowed-channels` | — | comma-separated channel ID (optional) |
| `OLA_DISCORD_API_BASE` | `--discord-api-base` | `https://discord.com/api/v10` | override สำหรับทดสอบกับ mock server |
| — | `--discord-max-concurrent` | `4` | จำนวนข้อความสูงสุดที่ประมวลผลพร้อมกันทั้งโปรเซส |
| `OLA_MAX_IMAGE_SIZE` | `--max-image-size` | `10M` | ขนาดรูปภาพสูงสุด — เหมือน `ola telegrambot` ทุกประการ |
| `OLA_SAVE_IMAGES` | `--save-images` | ปิด | บันทึกรูปไว้ดูซ้ำได้ — เหมือน `ola telegrambot` ทุกประการ |
| — | `-o, --output` | `discordbot.log` | log ไฟล์แบบเต็ม เปิดแบบ append เสมอ |

ตัวแปรที่เหลือทั้งหมด (`--persona`/`--persona-file`/`OLA_PERSONA`/`OLA_PERSONA_FILE`, `--knowledge-dir`/`OLA_KNOWLEDGE_DIR`, `--embed-model`/`OLA_EMBED_MODEL`/`--embed-top-k`/`--embed-min-score`/`--embed-refresh-interval`, `--context-dir`/`OLA_CONTEXT_DIR` (default ต่างกัน: `discord-context` ที่นี่ vs `telegram-context` ฝั่ง telegrambot ถ้าไม่ได้ตั้ง แต่**ตัวแปร env ตัวเดียวกัน** — ตั้ง `OLA_CONTEXT_DIR` แล้วสองบอทจะไปที่เดียวกันได้ถ้าต้องการ)/`--context-keep-recent`/`--context-compact-after`, การเชื่อมต่อโมเดล, web search/fetch ทั้งชุด) **ใช้ร่วมกับ `ola telegrambot` ทุก flag ทุกพฤติกรรมเป๊ะๆ** — ดู [`ola telegrambot`](#ola-telegrambot) และ [ตัวแปรสภาพแวดล้อมทั้งหมด](#ตัวแปรสภาพแวดล้อม-environment-variables-ทั้งหมด)

---

## `ola linebot`

LINE counterpart ของ `ola telegrambot`/`ola discordbot` — **หลักการ, toolset, persona, ฐานความรู้ (grep + embedding search), context compaction, grounding marker, group-recording+speaker attribution ทั้งหมดเหมือนกันทุกประการ** เพราะทั้งสามบอทใช้ `chatBotCore` ตัวเดียวกัน — **หัวข้อนี้จะพูดถึงเฉพาะส่วนที่ต่างจากสองบอทแรกเท่านั้น** ดูรายละเอียดส่วนที่เหมือนกันได้ที่ [`ola telegrambot`](#ola-telegrambot)

### ความต่างสำคัญที่สุด: LINE ยิง webhook เข้าหาเรา (ทิศทางตรงข้ามกับสองบอทแรก)

Telegram = long-poll ออกไปเอง (ไม่ต้องมี public endpoint), Discord = เปิด WebSocket ค้างไว้เอง (connection ออกไปเช่นกัน) — **LINE ตรงข้ามเลย**: ต้องมี **public HTTPS endpoint** ให้ LINE ยิง HTTP POST เข้ามาหาทุกครั้งที่มีข้อความ `ola linebot` รัน HTTP server เองด้วย `net/http` ตรงๆ (ไม่มี Go dependency ใหม่เลย ต่างจาก discordbot ที่ต้องมี `gorilla/websocket`) แต่**ต้องมี reverse proxy (เช่น Caddy/nginx) ที่จัดการ TLS ให้อยู่ข้างหน้า** เพราะตัว `ola linebot` เองฟังแค่ plain HTTP — เอา URL ของ reverse proxy (เช่น `https://yourdomain.com/line/webhook`) ไปตั้งใน LINE Developers Console → Messaging API → Webhook URL

ทุก request จาก LINE จะมี header `X-Line-Signature` แนบมาด้วยเสมอ (HMAC-SHA256 ของ request body คีย์ด้วย channel secret) — `ola linebot` ตรวจสอบทุก request ก่อนประมวลผล ถ้า signature ไม่ตรงจะตอบ `401` ทันทีโดยไม่แตะเนื้อหาเลย (ป้องกันคนอื่นปลอมแปลง request ยิงเข้ามาโดยไม่รู้ channel secret จริง)

### ความต่างสำคัญอันดับสอง: reply token หมดอายุเร็วเกินกว่าจะรอโมเดิลตอบ

ทุก webhook event ของ LINE มาพร้อม `replyToken` ที่ใช้ตอบได้**ครั้งเดียว**และ**หมดอายุเร็วมาก** (มีรายงานจากนักพัฒนาจริงว่าสั้นถึงราว 10 วินาที — LINE ไม่ได้ประกาศตัวเลขที่แน่นอนอย่างเป็นทางการ) สั้นกว่าเวลาที่โมเดิลใหญ่ใช้ตอบจริงมาก จึงออกแบบให้:
- คำตอบสำเร็จรูปที่ไม่รอโมเดิล (`/whoami`, `/tools`, ข้อความปฏิเสธ allowlist) → ใช้ **Reply API** (เร็วพอ, ฟรี) โดยมี Push เป็น fallback ถ้า token หมดอายุไปแล้วพอดี
- **คำตอบที่มาจากการเรียกโมเดิลจริง → ใช้ Push API เสมอ ไม่มีข้อยกเว้น** (ไม่ลอง Reply ก่อนเลย เพราะแทบจะหมดอายุแน่ๆ ตอนโมเดิลตอบเสร็จ) — ข้อแลกเปลี่ยนคือ push message นับรวมโควตารายเดือนของ LINE Official Account ในโมเดลบิลลิ่งแบบดั้งเดิม (reply message มักไม่นับ) ควรมอนิเตอร์ผ่าน LINE Official Account Manager ถ้าปริมาณการใช้งานเริ่มสูง

### ⚠️ ต้องเปิด "Allow bot to join group chats" ด้วยมือใน LINE Developers Console

เหมือนกับ Discord's Message Content Intent — ปิดโดย default ต้องไปเปิดเองที่ Console → Messaging API tab → Allow bot to join group chats **ลืมเปิดจุดนี้คือสาเหตุที่เชิญบอทเข้ากลุ่มไม่ได้เลย**

### เริ่มต้นใช้งาน

```bash
export OLA_LINE_CHANNEL_TOKEN='your-channel-access-token-from-line-developers-console'
export OLA_LINE_CHANNEL_SECRET='your-channel-secret'
export OLA_OLLAMA_MODEL=qwen3.6:27b

# ยังไม่รู้ user id ของตัวเอง? ทัก /whoami กับบอทหลังแอด friend แล้ว (ทำงานแม้ยังไม่อยู่ใน allowlist)
ola linebot --line-allowed-users U0000000000000000000000000000000

# ใช้งานจริง
ola linebot \
  --line-allowed-users U1111111111111111111111111111111 \
  --line-allowed-groups C2222222222222222222222222222222 \
  --persona 'คุณคือผู้ช่วยตอบคำถามวิชา Network Security ของภาควิชา ตอบสุภาพ กระชับ เป็นภาษาไทย' \
  --knowledge-dir /srv/course-docs/network-security \
  --embed-model bge-m3 \
  --line-listen-addr 127.0.0.1:8080
```

ตัวอย่างนี้ให้ `ola linebot` ฟังที่ `127.0.0.1:8080` เท่านั้น (ไม่ผูก public interface โดยตรง) แล้ววาง reverse proxy ไว้ข้างหน้าให้จัดการ TLS + forward มาที่นี่แทน — ปลอดภัยกว่าให้ `ola linebot` ฟัง public interface เองตรงๆ

### Allowlist สองชั้น (user + group — room ของเก่ารวมอยู่ในกลุ่มเดียวกับ group)

| Flag | ความหมาย |
|---|---|
| `--line-allowed-users` | user ID ที่แชท 1-on-1 ได้ (เหมือน `--telegram-allowed-users`) |
| `--line-allowed-groups` | group/room ID ที่บอทตอบได้ (LINE's "room" เป็นแนวคิดเก่าที่ LINE เองค่อยๆ รวมเข้ากับ "group" อยู่แล้ว จึงใช้ allowlist ชุดเดียวกัน ไม่แยกเหมือน Discord ที่มี channel เป็นชั้นที่สาม) |

### พฤติกรรมในกลุ่ม (group/room)

ตอบเฉพาะเมื่อถูก **@mention** (เช็คจาก `message.mention.mentionees` ที่ LINE ส่ง index/length ของแต่ละ mention มาให้แล้ว ไม่ได้ regex จาก text เอง) หรือขึ้นต้นด้วย `/ola`/`/ask`

### ชื่อผู้พูดในกลุ่มต้องเรียก API แยก (ต่างจาก Telegram/Discord)

Telegram (first name) และ Discord (username) ส่งชื่อผู้ส่งมาพร้อมกับข้อความในตัว webhook event เลย แต่ **LINE ส่งมาแค่ `userId` เปล่าๆ** — ต้องเรียก `GET /v2/bot/profile/{userId}` (แชท 1-on-1) หรือ `GET /v2/bot/group/{groupId}/member/{userId}` (สมาชิกในกลุ่ม) แยกต่างหากถึงจะได้ display name จริง เพราะฟีเจอร์บันทึกทุกข้อความในกลุ่ม (ดู [`ola telegrambot`](#ola-telegrambot)) ทำให้มีข้อความให้บันทึกถี่กว่าที่ต้องรู้ชื่อคนพูดบ่อยขึ้นมาก `ola linebot` เลย**แคชชื่อที่ดึงมาแล้วไว้ในหน่วยความจำตลอดอายุ process** กันเรียก API ซ้ำทุกข้อความจากคนเดิม (ชื่อคนแทบไม่เปลี่ยนบ่อย พอจะ cache ได้โดยไม่ refresh)

### ความจำต่อแชท

`line_user_<id>.json` (1-on-1), `line_group_<id>.json`/`line_room_<id>.json` (กลุ่ม) — คีย์ขึ้นต้นด้วย `line_` เสมอ ชนกับ Telegram/Discord ไม่ได้ **ใช้ `--context-dir` เดียวกับอีกสองบอทได้อย่างปลอดภัย** ถ้าอยากรันพร้อมกันหลายบอท

### Redelivery ของ webhook

LINE อาจส่ง webhook event ซ้ำได้เอง (เช่น ปัญหา network ระหว่างทาง) — `ola linebot` กันด้วย `webhookEventId` ที่ LINE รับประกันว่าเหมือนเดิมทุกครั้งที่ event เดียวกันถูกส่งซ้ำ ถ้าเจอ ID ที่เคยประมวลผลไปแล้วจะข้ามทันที ไม่เรียกโมเดิลซ้ำ ไม่ส่งข้อความซ้ำ

### Flags / Environment variables

| ตัวแปร | Flag | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `OLA_LINE_CHANNEL_TOKEN` | *(env เท่านั้น)* | — | **จำเป็น** — channel access token จาก LINE Developers Console |
| `OLA_LINE_CHANNEL_SECRET` | *(env เท่านั้น)* | — | **จำเป็น** — channel secret ใช้ตรวจสอบ signature ของ webhook request |
| `OLA_LINE_ALLOWED_USERS` | `--line-allowed-users` | — | comma-separated LINE user ID |
| `OLA_LINE_ALLOWED_GROUPS` | `--line-allowed-groups` | — | comma-separated LINE group/room ID |
| `OLA_LINE_API_BASE` | `--line-api-base` | `https://api.line.me/v2/bot` | override สำหรับทดสอบกับ mock server |
| `OLA_LINE_LISTEN_ADDR` | `--line-listen-addr` | `:8080` | ที่อยู่ที่ HTTP server ฟัง — แนะนำผูกแค่ `127.0.0.1:8080` แล้ววาง reverse proxy ไว้ข้างหน้า |
| `OLA_LINE_WEBHOOK_PATH` | `--line-webhook-path` | `/line/webhook` | path ของ webhook (ต้องตรงกับที่ตั้งใน LINE Developers Console) |
| — | `--line-max-concurrent` | `4` | จำนวนข้อความสูงสุดที่ประมวลผลพร้อมกันทั้งโปรเซส |
| `OLA_MAX_IMAGE_SIZE` | `--max-image-size` | `10M` | ขนาดรูปภาพสูงสุด — เหมือน `ola telegrambot` ทุกประการ |
| `OLA_SAVE_IMAGES` | `--save-images` | ปิด | บันทึกรูปไว้ดูซ้ำได้ — เหมือน `ola telegrambot` ทุกประการ |
| — | `-o, --output` | `linebot.log` | log ไฟล์แบบเต็ม เปิดแบบ append เสมอ |

ตัวแปรที่เหลือทั้งหมด (`--persona`/`--persona-file`, `--knowledge-dir`, `--embed-model`/`--embed-top-k`/`--embed-min-score`/`--embed-refresh-interval`, `--context-dir`/`OLA_CONTEXT_DIR` (default `line-context` ถ้าไม่ได้ตั้ง — ตัวแปรเดียวกับอีกสองบอท), `--context-keep-recent`/`--context-compact-after`, การเชื่อมต่อโมเดล, web search/fetch ทั้งชุด) **ใช้ร่วมกับ `ola telegrambot` ทุก flag ทุกพฤติกรรมเป๊ะๆ** — ดู [`ola telegrambot`](#ola-telegrambot) และ [ตัวแปรสภาพแวดล้อมทั้งหมด](#ตัวแปรสภาพแวดล้อม-environment-variables-ทั้งหมด)

---

## `ola webbot`

เว็บแอปแชทหน้าเดียว (HTML/CSS/JS ฝังอยู่ในตัวไบนารีเอง — ไม่มี build step, ไม่มี npm, ไม่มีไฟล์แยกให้ deploy คู่กัน) **toolset แบบ read-only เดียวกับ telegrambot/discordbot/linebot ทุกประการ ผ่าน `chatBotCore` ตัวเดียวกัน** (persona, ฐานความรู้, grounding marker ฯลฯ เหมือนกันหมด — ดูรายละเอียดที่ [`ola telegrambot`](#ola-telegrambot)) **ไม่ใช่ `ola ask` เวอร์ชันเว็บ** — ไม่มี `read_file`/`write_file`/`run_command` ให้เรียกเลย

### ความต่างจากสามบอทก่อนหน้า: ไม่มีแพลตฟอร์มภายนอกยืนยันตัวตนให้

Telegram/Discord/LINE ต่างมี "แพลตฟอร์ม" ที่ยืนยัน user identity มาให้ก่อนแล้ว เทียบกับ allowlist ได้เลย — **`webbot` ไม่มีสิ่งนี้** เพราะเป็น HTTP server ของ ola เองตรงๆ ป้องกันด้วยสองชั้น:

1. **ผูก `127.0.0.1` เป็นค่าเริ่มต้นเสมอ** (`--webbot-listen-addr`) ไม่ใช่ public interface — ถ้าจะให้เข้าถึงจากเน็ตได้จริง ต้องตั้งใจวาง reverse proxy ไว้ข้างหน้าเอง
2. **`OLA_WEBBOT_TOKEN`** (env เท่านั้น, ไม่มี flag — หลักการเดียวกับ token ของอีกสามบอท) — ถ้าตั้งไว้ ทุก request (ทั้งโหลดหน้าเว็บและเรียก API) ต้องผ่าน `?token=...` อย่างน้อยครั้งแรก (ระบบตั้ง cookie ให้เองหลังจากนั้น ไม่ต้องแปะ token ซ้ำทุกครั้ง) เทียบ token แบบ constant-time (`crypto/subtle`) กันการโจมตีแบบ timing attack ไม่ตั้งไว้ = ไม่มีการตรวจสอบสิทธิ์เลย (พึ่ง network exposure อย่างเดียว)

### ไม่เก็บบทสนทนาลงดิสก์เลย — ตามที่ตั้งใจออกแบบไว้

แต่ละ browser session ได้ `chatContext` ของตัวเองที่อยู่ใน**หน่วยความจำล้วนๆ** ตลอดอายุ session — ไม่มีการเรียก `.save()` ที่ไหนในโค้ดส่วนนี้เลยสักที่ ปิดแท็บหรือรีสตาร์ท process แล้วบทสนทนานั้นหายไปทันที (ตรงข้ามกับ telegrambot/discordbot/linebot ที่การหายไปแบบนี้ถือเป็นบั๊ก — ที่นี่คือพฤติกรรมที่ตั้งใจ) session ที่ไม่มีการใช้งานเกิน `--webbot-session-ttl` (default 2 ชั่วโมง) จะถูกเก็บกวาดทิ้งอัตโนมัติเป็นระยะ กัน memory โตไม่มีที่สิ้นสุดถ้ามีคนเปิดแท็บทิ้งไว้นานๆ หลายแท็บ

ถ้าเปิด `--embed-model` ด้วย ตัว knowledge index จะถูก cache ลง `--context-dir`/`OLA_CONTEXT_DIR` (default `webbot-context`) **เหมือนกับ telegrambot/discordbot/linebot ทุกประการ** — restart แล้วโหลด index เดิมกลับมา และ embed ใหม่เฉพาะไฟล์ที่เปลี่ยนแปลงเท่านั้น ไม่ต้อง embed ทั้งฐานความรู้ใหม่ทุกครั้งที่รีสตาร์ท **ข้อควรรู้:** `--context-dir` ของ webbot ใช้เก็บ**เฉพาะ** `knowledge-index.json` เท่านั้น ไม่มีบทสนทนาของผู้ใช้ถูกเขียนไว้ที่นี่เลย (บทสนทนายังคงอยู่ในหน่วยความจำล้วนๆ ตามเดิม)

### บอทแนะนำตัวเองก่อนเสมอ ก่อนผู้ใช้จะเริ่มพิมพ์ได้

ตามที่ขอไว้ — หน้าเว็บจะปิด input ไว้ก่อน แล้วเรียก `/api/session` ที่ให้**โมเดิลจริงสร้างคำแนะนำตัวเอง**ตามชื่อ/บุคลิกใน persona (ไม่ใช่ข้อความ hardcode ตายตัว) ก่อนจะเปิดให้พิมพ์ได้ ถ้าเรียกโมเดิลไม่สำเร็จ (เช่น backend ล่มชั่วคราว) จะ fallback เป็นข้อความทักทายสำรองแทน ไม่ปล่อยให้หน้าเว็บค้างรอเฉยๆ

### Theme และฟอนต์

**Gruvbox** (dark) และ **Playpen Sans Thai** (Google Font) ตามที่ระบุไว้ — โหลดฟอนต์ผ่าน `<link>` ไปยัง Google Fonts ตรงๆ (ต้องมีอินเทอร์เน็ตตอนเปิดหน้าเว็บถึงจะได้ฟอนต์ตามที่ตั้งใจ ไม่มีอินเทอร์เน็ตจะ fallback เป็น `sans-serif` ของเบราว์เซอร์แทนแต่หน้าเว็บยังใช้งานได้ปกติ)

### ปรับแต่งหน้าตา: `--webbot-title` และ `--webbot-avatar`

- **`--webbot-title`/`OLA_WEBBOT_TITLE`** — เปลี่ยนข้อความใน `<title>` และหัวหน้าเว็บ (default: `ola webbot`) ผ่าน `html/template` เสมอ (ไม่ใช่แทรก string ตรงๆ) ดังนั้นแม้ตั้ง title ที่มีอักขระ HTML พิเศษก็ปลอดภัย ไม่หลุดออกมาเป็น HTML/JS จริง
- **`--webbot-avatar`/`OLA_WEBBOT_AVATAR`** — ใส่รูปโปรไฟล์ของบอท แสดงทั้งข้างชื่อในหัวหน้าเว็บและข้างข้อความตอบของบอททุกข้อความ รับค่าได้ 2 แบบ:
  - **URL** — ขึ้นต้นด้วย `http://`, `https://`, หรือ `data:` จะใช้ตรงๆ ตามที่ให้มา
  - **path ไฟล์ในเครื่อง** — ถ้าไม่ใช่ URL ข้างบน จะถือว่าเป็น path ไฟล์รูปภาพ (PNG/JPEG/GIF/WebP) ในเครื่อง `ola` จะ**อ่านแล้วฝังเป็น `data:` URI ไว้ในหน้าเว็บให้เองตอนเริ่มทำงาน** ไม่ต้องเสิร์ฟไฟล์แยก ทำให้หน้าเว็บยังคง self-contained เหมือนเดิม — ข้อแลกเปลี่ยนคือเปลี่ยนไฟล์รูปแล้วต้อง restart `ola webbot` ใหม่ถึงจะเห็นรูปที่อัปเดต (เหมือนกับ `--webbot-title` ที่ก็ไม่ hot-reload เช่นกัน)

```bash
ola webbot \
  --webbot-title 'น้องโอลา ผู้ช่วยวิชา Network Security' \
  --webbot-avatar /home/user/ola-avatar.png
```

### เริ่มต้นใช้งาน

```bash
export OLA_OLLAMA_MODEL=qwen3.6:27b

# ใช้งานแบบง่ายที่สุด (localhost เท่านั้น ไม่มี token)
ola webbot --persona 'คุณคือผู้ช่วยตอบคำถามวิชา Network Security ของภาควิชา' --knowledge-dir /srv/course-docs

# เปิดให้เข้าถึงได้กว้างกว่า localhost ผ่าน reverse proxy (แนะนำตั้ง token เสมอถ้าทำแบบนี้)
export OLA_WEBBOT_TOKEN='เลือกวลีที่เดายากเอง'
ola webbot \
  --persona 'คุณคือผู้ช่วยตอบคำถามวิชา Network Security ของภาควิชา' \
  --knowledge-dir /srv/course-docs \
  --embed-model bge-m3 \
  --searxng-url http://127.0.0.1:3001 \
  --webbot-title 'ผู้ช่วยวิชา Network Security' \
  --webbot-avatar /srv/course-docs/avatar.png \
  --webbot-listen-addr 127.0.0.1:8090
```

เปิดเบราว์เซอร์ไปที่ `http://127.0.0.1:8090/` (หรือ `http://127.0.0.1:8090/?token=...` ถ้าตั้ง token ไว้) — ตัวอย่างนี้ตั้งใจให้ `ola webbot` ฟังแค่ `127.0.0.1` เท่านั้น แล้ววาง reverse proxy (Caddy/nginx) ไว้ข้างหน้าจัดการ TLS + forward เข้ามาแทนถ้าจะเปิดให้เข้าถึงจากเน็ตจริง — ปลอดภัยกว่าให้ `ola webbot` ฟัง public interface เองตรงๆ

### Flags / Environment variables

| ตัวแปร | Flag | ค่าเริ่มต้น | หมายเหตุ |
|---|---|---|---|
| `OLA_WEBBOT_TOKEN` | *(env เท่านั้น)* | — | shared token gate — ไม่ตั้ง = ไม่มีการตรวจสอบสิทธิ์เข้าถึงเลย |
| `OLA_WEBBOT_LISTEN_ADDR` | `--webbot-listen-addr` | `127.0.0.1:8090` | ที่อยู่ที่ HTTP server ฟัง |
| — | `--webbot-session-ttl` | `7200` วินาที (2 ชม.) | อายุ session สูงสุดก่อนถูกเก็บกวาดทิ้งถ้าไม่มีการใช้งาน |
| `OLA_WEBBOT_TITLE` | `--webbot-title` | `ola webbot` | ข้อความใน `<title>` และหัวหน้าเว็บ |
| `OLA_WEBBOT_AVATAR` | `--webbot-avatar` | — | URL (`http(s)://`/`data:`) หรือ path ไฟล์รูปภาพในเครื่อง (ฝังเป็น `data:` URI ให้อัตโนมัติ) |
| `OLA_MAX_IMAGE_SIZE` | `--max-image-size` | `10M` | ขนาดรูปภาพสูงสุด — เหมือน `ola telegrambot` ทุกประการ |
| — | `-o, --output` | `webbot.log` | log ไฟล์แบบเต็ม เปิดแบบ append เสมอ |

ตัวแปรที่เหลือทั้งหมด (`--persona`/`--persona-file`, `--knowledge-dir`, `--embed-model`/`--embed-top-k`/`--embed-min-score`/`--embed-refresh-interval`, `--context-dir`/`OLA_CONTEXT_DIR` (default `webbot-context` — เก็บเฉพาะ knowledge index เท่านั้น ไม่มีบทสนทนา), `--context-keep-recent`/`--context-compact-after` (compact บทสนทนาในหน่วยความจำเท่านั้น), การเชื่อมต่อโมเดล, web search/fetch ทั้งชุด) **ใช้ร่วมกับ `ola telegrambot` ทุก flag ทุกพฤติกรรมเป๊ะๆ** — ดู [`ola telegrambot`](#ola-telegrambot) และ [ตัวแปรสภาพแวดล้อมทั้งหมด](#ตัวแปรสภาพแวดล้อม-environment-variables-ทั้งหมด) **ไม่มี** flag allowlist แบบ `--*-allowed-users`/`--*-allowed-groups` เหมือนสามบอทก่อน เพราะไม่มีแนวคิด "ผู้ใช้แต่ละคน" ให้ allowlist — การควบคุมการเข้าถึงทำผ่าน network binding + token เท่านั้น

---

## Provider: ollama vs openai-compatible

เลือกด้วย `-P/--provider` หรือ `$OLA_PROVIDER` (default: `ollama`)

- **`ollama`** (default) — พฤติกรรมเดิมของ ola ทุกอย่าง คุยกับ Ollama's native `/api/chat` โดยตรง
- **`openai`** — คุยกับ endpoint ใดก็ได้ที่พูด OpenAI chat-completions wire format (`<host>/chat/completions`) ใช้ได้ทั้ง OpenAI จริง, llama.cpp server, vLLM, LM Studio, text-generation-webui หรือแม้แต่ endpoint `/v1` ในตัวของ Ollama เอง — host default เมื่อไม่ตั้ง `--api-base`/`OLA_OPENAI_API_BASE` คือ `http://localhost:11434/v1` (ชี้เข้า Ollama ที่รันอยู่แล้วนั่นเอง จึงสลับ provider ได้ทันทีโดยไม่ต้องตั้งอะไรเพิ่ม)

Tool/system-prompt/sandboxing/verify/quiet-mode/notification ทำงานเหมือนกันทุกประการไม่ว่าจะใช้ provider ไหน — เปลี่ยนแค่รูปแบบ request/response บน wire เท่านั้น

**ข้อจำกัดที่รู้อยู่แล้ว 2 อย่างของ `openai`:**
1. `num_ctx` ไม่ถูกส่งเลย เพราะไม่มี field มาตรฐานเทียบเท่าใน OpenAI wire format
2. `-T/--no-think` ไม่มีผลใด ๆ เพราะไม่มี field มาตรฐานกลางสำหรับปิด reasoning (ola จะแสดง warning แทนที่จะทำเนียนว่าปิดได้)

```bash
# ตัวอย่าง: ชี้ไปยัง LM Studio ที่เปิด OpenAI-compatible server ไว้ที่พอร์ต 1234
ola ask -P openai --api-base http://localhost:1234/v1 -m local-model 'สรุปโค้ดนี้'
```

---

## Web search / web fetch

- **`web_fetch`** เปิดอัตโนมัติเสมอ ไม่ต้องตั้งค่าอะไร — ยิง HTTP GET ธรรมดา (native `net/http`, ไม่มี Playwright/เบราว์เซอร์เสริม) แล้วตัด HTML เหลือแต่ข้อความ **ไม่รัน JavaScript ไม่ว่ากรณีใด** — หน้า SPA ที่ render ด้วย JS ล้วนจะได้ error ที่บอกชัดเจนแทนผลลัพธ์ว่าง/ไม่ครบ
- **`web_search`** ปิดโดย default จนกว่าจะตั้งค่า backend ใดบ้างหนึ่ง (ถ้าตั้งทั้งคู่ **SearXNG ชนะเสมอ**):
  1. `OLA_OLLAMA_SEARCH_API_KEY` หรือ `$OLLAMA_API_KEY` (หรือ `--ollama-search-key`) → เรียก Ollama's hosted Web Search API (`https://ollama.com/api/web_search`) ไม่ต้องรัน service เพิ่มเอง แค่มี API key จาก [ollama.com/settings/keys](https://ollama.com/settings/keys)
  2. `OLA_SEARXNG_API_BASE` (หรือ `--searxng-url`) → เรียก local SearXNG instance ผ่าน JSON API (ต้องเปิด `formats: json` ใน `settings.yml` ของ SearXNG ก่อน — ดูหัวข้อถัดไป)

ทั้งสอง tool รับ list ของ query/url ได้ในเรียกเดียว ยิงแบบขนาน (bounded concurrency) อัตโนมัติ ปิดทั้งคู่พร้อมกันได้ด้วย `--no-web-search`

```bash
# วิธีที่ 1: ใช้ Ollama hosted search
export OLA_OLLAMA_SEARCH_API_KEY=sk-xxxxx
ola ask 'ค้นข่าว AI ล่าสุด 3 วันนี้แล้วสรุปให้หน่อย'

# วิธีที่ 2: ใช้ SearXNG ของตัวเอง (ดูหัวข้อถัดไปสำหรับการตั้งค่า)
ola ask --searxng-url http://localhost:3001 'ค้นข่าว AI ล่าสุด 3 วันนี้แล้วสรุปให้หน่อย'
```

---

## ตั้งค่า SearXNG ด้วย `websearch.yml`

`websearch.yml` เป็น **Docker Swarm stack file** สำหรับรัน [SearXNG](https://github.com/searxng/searxng) (meta search engine โอเพนซอร์ส) แบบ self-hosted เพื่อเป็น backend ของ `web_search`

```yaml
version: "3.8"

services:
  searxng:
    image: searxng/searxng:latest
    ports:
      - 127.0.0.1:3001:8080   # เปิดเฉพาะ localhost เท่านั้น ไม่ expose ออกนอกเครื่อง
    volumes:
      - ./searxng:/etc/searxng:rw
    environment:
      - SEARXNG_BASE_URL=http://searxng:8080/
      - SEARXNG_SECRET=${SEARXNG_SECRET:-my_super_secret_key_change_me}
    cap_drop: [ALL]
    cap_add: [CHOWN, SETGID, SETUID, DAC_OVERRIDE]
    networks: [ai-net]
    deploy:
      replicas: 1
      restart_policy: { condition: on-failure }
      placement:
        constraints:
          - node.role == manager   # ต้องรันบน Manager node เพราะใช้ bind mount

networks:
  ai-net:
    driver: overlay
    attachable: true
```

**ประเด็นสำคัญของไฟล์นี้:**
- ใช้ `deploy:` block → ต้องรันผ่าน **`docker stack deploy`** (Docker Swarm mode) ไม่ใช่ `docker compose up` ธรรมดา
- `placement.constraints: node.role == manager` **บังคับ** เพราะ bind mount (`./searxng:/etc/searxng`) ต้องอิงโฟลเดอร์บนเครื่อง manager เท่านั้น ถ้า service ถูก schedule ไปลง worker node อื่น โฟลเดอร์นี้จะไม่มีอยู่
- พอร์ตผูกไว้ที่ `127.0.0.1:3001` เท่านั้น (ไม่ bind `0.0.0.0`) เพื่อไม่ให้เข้าถึงจากนอกเครื่อง — ola จะเรียกผ่าน `http://localhost:3001`
- `cap_drop: ALL` แล้วเปิดเฉพาะ capability ที่จำเป็นจริง ๆ กลับมา (`CHOWN`, `SETGID`, `SETUID`, `DAC_OVERRIDE`) เป็นแนวทาง least-privilege
- network `ai-net` เป็น overlay network แบบ `attachable: true` — service/container อื่นที่ attach เข้า network นี้เรียก SearXNG ผ่านชื่อ `searxng:8080` (internal DNS ของ Swarm) ได้โดยตรง

**ขั้นตอนติดตั้ง:**

```bash
# 1) เริ่ม Swarm mode (ถ้ายังไม่เคยเปิด) — รันครั้งเดียวบนเครื่องที่จะเป็น manager
docker swarm init

# 2) เตรียมโฟลเดอร์ config ของ SearXNG ให้ตรงกับ bind mount
mkdir -p ./searxng

# 3) (แนะนำ) ตั้ง secret ของตัวเองแทนค่า default ในไฟล์
export SEARXNG_SECRET=$(openssl rand -hex 32)

# 4) deploy stack
docker stack deploy -c websearch.yml websearch

# 5) ตรวจสอบว่า container รันขึ้นจริง
docker service ls
docker service logs websearch_searxng
```

หลัง container รันขึ้นครั้งแรก จะมีไฟล์ `settings.yml` ถูกสร้างใน `./searxng/settings.yml` (ที่ mount ไว้) — **ต้องแก้ให้เปิด JSON API** ก่อน `ola` จะเรียกใช้งานได้ โดยเพิ่ม `json` เข้า `formats` ใน section `search:`:

```yaml
search:
  formats:
    - html
    - json
```

จากนั้น restart service (`docker service update --force websearch_searxng`) แล้วชี้ ola ไปที่พอร์ตที่ publish ไว้:

```bash
export OLA_SEARXNG_API_BASE=http://localhost:3001
ola ask 'ค้นเว็บหาราคาทองคำวันนี้'
```

> **หมายเหตุ:** ถ้าไม่ได้ใช้ Docker Swarm (ใช้ `docker compose` ธรรมดา) ให้ตัด block `deploy:` ทั้งหมดออก แล้วใช้ `docker compose -f websearch.yml up -d` แทนได้ — แค่จะไม่มี `restart_policy`/`placement constraint` ให้ ต้องจัดการ restart เองผ่าน `restart: unless-stopped` แทน

---

## Skills system

เปิดใช้เมื่อระบุ `--skills-dir`/`OLA_SKILLS_DIR` เท่านั้น (default: ปิด, ไม่มีผลกระทบใด ๆ ถ้าไม่ตั้ง)

- แต่ละ subdirectory ที่มีไฟล์ `SKILL.md` อยู่ข้างใน จะถูกโหลดเป็น "skill" หนึ่งตัว — รองรับทั้งแบบตรง (`<dir>/<skill>/SKILL.md`) และแบบแบ่งหมวดหมู่หนึ่งชั้น (`<dir>/<category>/<skill>/SKILL.md` เช่น `/mnt/skills/public/pptx` — โครงสร้างเดียวกับ skill system ของ Claude เอง) ผสมกันได้ในไดเรกทอรีเดียวกัน และตามลิงก์ (symlink) ได้ทั้ง skill directory และ category directory
- มีแค่ **ชื่อ + คำอธิบายสั้น ๆ** ของแต่ละ skill เท่านั้นที่ถูกแปะเข้า system prompt อัตโนมัติ (หัวข้อ "AVAILABLE SKILLS") — เนื้อหาเต็มไม่ถูกโหลดเข้า context ทันที โมเดิลต้องเรียก tool `read_skill` เองเมื่อเห็นว่าเกี่ยวข้องกับงาน (หลักการเดียวกับ read_file ก่อน edit_file)
- ระบุได้หลาย directory พร้อมกันด้วย comma คั่น เช่น `/mnt/skills/public,/mnt/skills/private` — สแกนตามลำดับที่ระบุ ถ้าชื่อ skill ซ้ำกัน directory แรกที่เจอชนะ ตัวที่ซ้ำจะถูกข้ามพร้อม warning
- **รูปแบบ `SKILL.md`:** เริ่มไฟล์ด้วย frontmatter บรรทัด `key: value` ระหว่าง `---` สองบรรทัดได้ (`name:`, `description:` — ไม่ใช่ YAML เต็มรูปแบบ) ถ้าไม่มี frontmatter จะ fallback ไปใช้ชื่อ directory เป็นชื่อ skill และบรรทัดข้อความแรกในไฟล์เป็นคำอธิบาย

```bash
ola ask --skills-dir /mnt/skills/public,/mnt/skills/private 'สร้างสไลด์สรุปบทที่ 5'
```

---

## `scp_copy` — โอนไฟล์ข้าม host

เปิดใช้เมื่อระบุ `--scp-hosts`/`OLA_SCP_HOSTS` เท่านั้น ใช้ `scp` binary ของระบบเรียกตรงผ่าน argv (ไม่ผ่าน `sh -c`) — **โมเดิลเลือกได้แค่ `remote_alias` จากรายชื่อที่ตั้งไว้ล่วงหน้าเท่านั้น** ไม่มีทางระบุ user/host/port/remote path เองได้เลย

รูปแบบ: `"alias=user@host[:port]/remote/root"` คั่นหลาย host ด้วย comma:

```bash
export OLA_SCP_HOSTS="backup=moo@10.0.0.5:22/srv/backup,nas=moo@nas.local/mnt/data"
```

- ทั้งฝั่ง local (`--scp-local-dir`, default: current directory) และฝั่ง remote (root ต่อ alias) ถูก sandbox แบบเดียวกับ `read_file`/`write_file`
- **Auth:** ใช้ SSH key ที่ config ไว้แล้วในเครื่องเท่านั้น (ssh-agent/`~/.ssh/config` หรือ `--scp-key`/`OLA_SCP_KEY` ระบุ identity file เพิ่มได้) **ไม่รองรับ password ใด ๆ ทั้งสิ้น** รันด้วย `BatchMode=yes` + `StrictHostKeyChecking=yes` เสมอ (ไม่ prompt ไม่ bypass host-key verification)
- ไม่มีการถาม `ask_user` ก่อนรัน — เรียกแล้วทำทันที (เหมือน `write_file`/`edit_file`) ความปลอดภัยอยู่ที่ allowlist/sandbox ไม่ใช่การขอ confirm ทุกครั้ง
- จำกัดขนาดไฟล์ต่อครั้งด้วย `--scp-max-bytes` (default 100MB) และ timeout ด้วย `--scp-timeout` (default 120s) ทุกครั้งที่โอนสำเร็จจะถูกบันทึกและส่ง ntfy.sh notification (ถ้าตั้ง `-x/OLA_TOPIC`)

```bash
ola ask --scp-hosts 'backup=moo@10.0.0.5/srv/backup' 'สำรอง report.txt ไปที่ backup หน่อย'
```

---

## `api_request` — เรียก HTTP API

เปิดใช้เมื่อระบุ `--api-endpoints`/`OLA_API_ENDPOINTS` **หรือ** `--api-allow-direct-url` เท่านั้น มีสองวิธีเลือกปลายทาง:

1. **`endpoint`** — โมเดิลเลือก `endpoint` เป็นชื่อ alias ที่ตั้งไว้ล่วงหน้าเท่านั้น (allowlist หลักการเดียวกับ `scp_copy`) รูปแบบ `"alias=https://base.url"` คั่นหลายตัวด้วย comma:
   ```bash
   export OLA_API_ENDPOINTS="ollama=http://localhost:11434,openwebui=http://localhost:8080"
   ```
   endpoint เท่านั้นที่เข้าถึง host ภายใน/private ได้ ถ้าต้องใช้ credential ตั้งแยกผ่าน `OLA_API_ENDPOINT_<ALIAS>_AUTH_HEADER`/`_AUTH_VALUE` — ola แนบ header นี้ให้เองทุกครั้ง โดยที่**โมเดิลไม่เห็นค่าจริงเลย** ไม่ว่าใน tool call หรือ log ไฟล์ `-o`
2. **`url`** — ระบุ URL ตรงเหมือน `web_fetch` (เฉพาะเมื่อเปิด `--api-allow-direct-url`) ผ่าน SSRF guard เดียวกับ `web_fetch` เสมอ (ปฏิเสธ private/reserved IP และ localhost)

- `GET`/`HEAD`/`OPTIONS` ใช้ได้เสมอเมื่อเปิด tool นี้ ส่วน `POST`/`PUT`/`PATCH`/`DELETE` ต้องเปิด `--api-allow-mutating` เพิ่มอีกชั้น (default ปิด กันเรียก API ที่มีผลข้างเคียงโดยไม่ตั้งใจ)
- รองรับ query/headers เพิ่มเติม (header สงวน เช่น `Authorization`/`Host` ถูกข้ามเสมอ — ถ้าต้องใช้ auth ให้ตั้งที่ endpoint แทน) body รองรับ json/form/multipart/text/binary/none ผ่าน `body_type`
- response ที่ไม่ใช่ 2xx ไม่ถือเป็น error — คืน status code + เนื้อหากลับให้โมเดิลตัดสินใจเอง
- ทุกครั้งที่เรียกด้วย method mutating สำเร็จ จะถูกบันทึกเข้า session change log และส่ง ntfy.sh notification (ถ้าตั้ง `-x/OLA_TOPIC`)

```bash
ola ask --api-endpoints 'ollama=http://localhost:11434' 'เช็คว่ามีโมเดลอะไรบ้างใน ollama ตอนนี้'
```

---

## Quiet mode

เปิดด้วย `-q/--quiet` หรือ `$OLA_QUIET` (default: ปิด) ตัดสิ่งที่ ola พิมพ์ลง terminal ให้เหลือแค่ 2 อย่างที่ต้องเห็นสด ๆ จริง ๆ: **คำตอบสุดท้ายของโมเดิล** และ **คำถาม/ตัวเลือกของ `ask_user`** (ยังต้องแสดงเสมอ เพราะเป็นทางเดียวปลดล็อกเซสชันที่ค้างรอ)

| สิ่งที่ถูกซ่อนจาก terminal (ยังบันทึกครบใน `-o` log เสมอ) |
|---|
| tool_call แต่ละครั้งและ preview ผลลัพธ์ (🔧/✓/✗) |
| thinking banner + thinking token ที่ stream สด ๆ |
| บรรทัด timing ต่าง ๆ (load, preload, prompt eval, round, tokens, verify progress) |
| สรุปผล web_search (จำนวนผลลัพธ์ + รายชื่อ) |

เมื่อเซสชันหยุดกลางคันแบบผิดปกติ (ชน iteration/verify cap) ข้อความเตือนไปออกที่ **stderr** แทน stdout แทนที่จะหายไปเฉย ๆ — `-n/--dry-run` ไม่ได้รับผลกระทบจาก `-q` เลย (ยังแสดงรายละเอียดเต็มเสมอ)

---

## ntfy.sh push notifications

ใช้ `-x <topic>` หรือตั้ง `$OLA_TOPIC` เพื่อรับ push notification ผ่าน [ntfy.sh](https://ntfy.sh) แจ้งเตือนครอบคลุม: งานเสร็จ/error, เขียนไฟล์ (`[WRITE]`), แก้ไฟล์ (`[EDIT]`), `[MKDIR]`, `scp_copy`, `api_request` (mutating), รอคำตอบ (`[ASK]`), และ (เฉพาะ `coding`) ปิด task สำเร็จ (`[TASK]`)

ถ้าเปิด `-q/--quiet` ไว้ด้วย จะเหลือแค่ `[ASK]` กับตอนจบงาน (`Work Finished`/`Failed`) เท่านั้น — notification ระหว่างทางอื่น ๆ ถูกงดไว้

```bash
ola ask -x mytopic 'deploy to production'
```

---

## ไฟล์แพลตฟอร์ม

`ola` แยก terminal/process-group helper ตาม OS ผ่าน Go build tag:

- **`platform_linux.go`** (`//go:build linux`) — เช็ค terminal จริงด้วย `ioctl(TCGETS)` (แยกแยะ `/dev/null` จาก tty จริงได้ ป้องกัน `ask_user` ค้างรอ input ที่ไม่มีวันมาในงาน cron/redirect) และตั้ง process group (`Setpgid`) ก่อนรันคำสั่ง เพื่อให้ `killProcessGroup` ฆ่าได้ทั้ง process group (รวม grandchild ที่ build/test command อาจ fork ออกมา) ไม่ใช่แค่ตัวลูกโดยตรง — จำเป็นสำหรับให้ `--cmd-timeout` ทำงานได้จริง
- **`platform_other.go`** (`//go:build !linux`) — fallback แบบ best-effort สำหรับ macOS/Windows/BSD: เช็ค terminal ด้วย `os.ModeCharDevice` ธรรมดา (แยก `/dev/null` ไม่ได้แม่นเท่า Linux) และ kill ได้แค่ process ลูกโดยตรง เพราะ **เป้าหมายหลักของ ola คือ Linux** ไฟล์นี้จึงมีไว้ให้ build ผ่านบน OS อื่นเท่านั้น ไม่ได้ optimize เท่าฝั่ง Linux

---

## การรันเทสต์

เทสต์ทั้งหมดของโปรเจกต์รวมอยู่ใน `main_test.go` ไฟล์เดียว (unit tests แบบไม่พึ่งเครือข่าย + end-to-end tests ที่ยิงผ่าน `cmdAsk`/`cmdCoding` จริงเข้าไปยัง mocked Ollama `/api/chat` ด้วย `httptest`):

```bash
go test ./...

# แบบละเอียด
go test -v ./...

# รันเทสต์เฉพาะกลุ่ม (ชื่อ match ด้วย regex)
go test -run TestCodingQuietMode -v ./...
```

---

## ข้อจำกัด/สิ่งที่ควรรู้

- **`run_command` ไม่ใช่ sandbox จริง** — denylist และขอบเขต working-directory-only (ดู [กลไก run_command](#กลไก-run_command-เปิดเสมอ)) เป็นการตรวจแบบ text/regex เท่านั้น ป้องกันความผิดพลาดที่พบบ่อยและเสียหายมาก ไม่ใช่การรับประกันความปลอดภัยสมบูรณ์ต่ออินพุตที่จงใจหลบเลี่ยง (เช่น command substitution หรือ script ที่ถูกเรียกโดยชื่อ) — ใช้ให้เหมาะกับความไว้ใจที่มีต่อโมเดิล/งานที่ทำ
- **`ask_user` ต้องมี stdin เป็น terminal จริง** — ใช้กับ script/cron/pipe (non-interactive) ไม่ได้ ถ้าโมเดิลเรียกจะได้ error แทน (ดี สำหรับ `ola coding` ที่ตั้งใจรันแบบไม่มีคนเฝ้า เพราะมันจะเลือก assumption เองแทนการค้างรอ)
- **Tool-calling loop มีเพดาน** — `ola ask` สูงสุด 25 รอบ, `ola coding` สูงสุด 300 รอบ (`--max-iterations`) หรือ 3 ชม. (`--max-duration`) แล้วแต่อันไหนถึงก่อน
- **System prompt แก้จากภายนอกไม่ได้** — ไม่มี `-s/--system` อีกต่อไป ปรับพฤติกรรมได้ผ่าน flag/env ที่มีให้เท่านั้น (หรือแก้ constant ในซอร์สแล้ว build ใหม่)
- **`web_fetch` ไม่รัน JavaScript** — หน้าเว็บที่เป็น client-side-rendered SPA ล้วนจะได้ error ชัดเจน ไม่ใช่เนื้อหาว่างเปล่า
- **ไม่รองรับ auto-detect .gitignore** สำหรับ directory tree ที่แปะเข้า prompt แรก — ยกเว้นเฉพาะโฟลเดอร์ที่รู้จักแบบ hardcode (`.git`, `node_modules`, `vendor`, `target`, `.venv`, `__pycache__`, `dist`, `build`, `.idea`, `.terraform` ฯลฯ) อาจไม่ตรงกับทุกโปรเจกต์เป๊ะ
- **อ่าน PDF (ทั้ง `[files...]` และ tool `read_pdf`) ต้องมี `pdftoppm` ติดตั้งอยู่ และโมเดลที่ใช้ต้องรองรับ vision** — ola แปลง PDF เป็นภาพรายหน้าแล้วส่งแบบเดียวกับไฟล์รูป ไม่มีการดึงข้อความ (text extraction) หรือ OCR ใด ๆ ทั้งสิ้น ถ้าโมเดิลไม่รองรับ vision ภาพที่ส่งไปจะไม่ถูกนำไปใช้ประโยชน์ (ola ไม่เช็คความสามารถของโมเดลให้ล่วงหน้า) ดู [กลไกอ่าน PDF](#กลไกอ่าน-pdf)
