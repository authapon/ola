# ola

**ola** เป็น CLI (คำสั่งเดียว, ไบนารีเดียว, เขียนด้วย Go ล้วน ไม่พึ่งพา `curl`/`jq`/`perl`/`base64` ภายนอก) สำหรับคุยกับ LLM ผ่าน Ollama (หรือ endpoint แบบ OpenAI-compatible ใดก็ได้) พร้อม **tool calling ที่เปิดใช้งานเสมอ** — โมเดลไม่ได้แค่ตอบข้อความ แต่ *อ่าน/เขียน/แก้ไฟล์จริงบนดิสก์*, ค้นเว็บ, เรียก API, โอนไฟล์ข้าม host, และถามคุณกลับเมื่อจำเป็น ทั้งหมดนี้ sandbox อยู่ใน current directory ที่คุณรัน `ola` เท่านั้น

โปรเจกต์นี้มีสองคำสั่งย่อย:

| คำสั่ง | ใช้เมื่อไหร่ |
|---|---|
| [`ola ask`](#ola-ask) | ถามคำถามครั้งเดียว มี human คอยตอบโต้ระหว่างทางได้ (เหมือนคุยกับ AI assistant ทั่วไป แต่มันแก้ไฟล์ให้จริง) |
| [`ola coding`](#ola-coding) | ให้ทำงานยาว ๆ แบบไม่มีคนเฝ้า: ป้อนไฟล์ requirements แล้วปล่อยให้มันวางแผน → เขียนโค้ด → build/test เอง → แก้จนผ่านจริง |

> ทั้งสองคำสั่งพูดได้สองภาษา (protocol): Ollama's native `/api/chat` (ค่าเริ่มต้น) หรือ endpoint แบบ OpenAI chat-completions (`-P openai`) — ดู [Provider](#provider-ollama-vs-openai-compatible)

---

## สารบัญ

1. [ภาพรวมและปรัชญาการออกแบบ](#ภาพรวมและปรัชญาการออกแบบ)
2. [การติดตั้ง](#การติดตั้ง)
3. [เริ่มต้นใช้งานอย่างเร็ว](#เริ่มต้นใช้งานอย่างเร็ว)
4. [ตัวแปรสภาพแวดล้อม (Environment Variables) ทั้งหมด](#ตัวแปรสภาพแวดล้อม-environment-variables-ทั้งหมด)
5. [`ola ask`](#ola-ask)
6. [`ola coding`](#ola-coding)
7. [Provider: ollama vs openai-compatible](#provider-ollama-vs-openai-compatible)
8. [Web search / web fetch](#web-search--web-fetch)
9. [ตั้งค่า SearXNG ด้วย `websearch.yml`](#ตั้งค่า-searxng-ด้วย-websearchyml)
10. [Skills system](#skills-system)
11. [scp_copy — โอนไฟล์ข้าม host](#scp_copy--โอนไฟล์ข้าม-host)
12. [api_request — เรียก HTTP API](#api_request--เรียก-http-api)
13. [Quiet mode](#quiet-mode)
14. [ntfy.sh push notifications](#ntfysh-push-notifications)
15. [ไฟล์แพลตฟอร์ม (`platform_linux.go` / `platform_other.go`)](#ไฟล์แพลตฟอร์ม)
16. [การรันเทสต์](#การรันเทสต์)
17. [ข้อจำกัด/สิ่งที่ควรรู้](#ข้อจำกัดสิ่งที่ควรรู้)

---

## ภาพรวมและปรัชญาการออกแบบ

- **Tool calling ไม่มีสวิตช์ปิด** — ทุก request ที่ส่งไป Ollama แนบ tool schema ไปด้วยเสมอ แล้ววนลูปเรียกโมเดล → รัน tool ที่โมเดลขอ → ป้อนผลลัพธ์กลับ จนกว่าโมเดลจะตอบเป็นข้อความปกติ (หรือชนเพดานจำนวนรอบ)
- **ไม่มี `--workdir`** — ทุก path ที่ tool อ้างอิงคือ current directory ที่รัน `ola` เสมอ และไม่มีทางหลุดออกไปนอก directory นั้นได้ (ทั้ง absolute path และ `..` จะถูกปฏิเสธ)
- **เขียนไฟล์จริง ไม่ใช่ข้อความให้ copy-paste** — ola รุ่นเก่าเคยมีกลไก marker พิเศษ (`<<<ooo FILENAME ooo>>> ... <<<xxx FILENAME xxx>>>`) กับคำสั่งย่อย `extract` ให้มนุษย์ค่อยแยกไฟล์เอาทีหลัง กลไกนั้นถูกถอดออกไปแล้ว — ตอนนี้ `write_file`/`edit_file` แก้ไฟล์บนดิสก์ทันที
- **System prompt คงที่ ตายตัวในไบนารี** — ไม่มี `-s/--system` ให้เปลี่ยนจากภายนอกอีกต่อไป เพราะ contract ของ tool calling (tool มีอะไรบ้าง, sandbox ยังไง, เมื่อไหร่ต้องถาม user) สำคัญเกินกว่าจะให้ override แบบเสี่ยง prompt พังตอนรันจริง ข้อยกเว้นเดียวคือส่วน "AVAILABLE SKILLS" ที่ *เติมต่อ* ท้าย prompt เมื่อตั้งค่า skills เท่านั้น (ดู [Skills system](#skills-system))
- **ไม่เชื่อคำพูดโมเดิลเปล่า ๆ** — เมื่อโมเดิลแก้ไฟล์โค้ด ola จะรัน build/test ของโปรเจกต์เองอย่างอิสระอีกครั้งก่อนยอมรับว่า "เสร็จ" (`ola ask`) หรือบังคับ gate หลายชั้น (`ola coding`) แทนที่จะเชื่อว่าโมเดิลพูดว่า "compiles/passes tests" แล้วจบ
- **โครงสร้างซอร์สโค้ด** — โปรเจกต์รวมไฟล์ทั้งหมดเหลือน้อยไฟล์ (file-count cleanup): `main.go` (entry point + tool-calling loop ของทั้งสองคำสั่งย่อย + integrations ทั้งหมด), `main_test.go` (เทสต์ทั้งหมด), และ `platform_linux.go`/`platform_other.go` ที่แยกเฉพาะโค้ดที่ผูกกับ build tag (`//go:build linux` / `//go:build !linux`) เพราะไฟล์แบบ build-tag ต้องมี "เฉพาะ" โค้ดที่ตรงเงื่อนไขเท่านั้น เลยรวมเข้า `main.go` ไม่ได้

---

## การติดตั้ง

**ข้อกำหนด:** Go **1.26.2** ขึ้นไป (ตาม `go.mod`) — ไม่มี dependency ภายนอกอื่นเลย (มาตรฐาน stdlib ล้วน)

```bash
# 1) เตรียมโฟลเดอร์โปรเจกต์ (main.go, main_test.go, go.mod, platform_linux.go, platform_other.go ต้องอยู่ที่เดียวกัน)
cd /path/to/ola

# 2) build เป็นไบนารีชื่อ ola
go build -o ola .

# 3) (แนะนำ) ย้ายเข้า PATH
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

ถ้าไม่แนบไฟล์ใด ๆ เลย ola จะสแกน current directory ทั้งหมด (recursive, ยกเว้น `.git`/`node_modules`/`vendor`/build artifact ต่าง ๆ) แล้วแปะ directory tree เข้า prompt แรกให้อัตโนมัติ โมเดิลจะเห็น scope ของโปรเจกต์ทันทีโดยไม่ต้องเสีย tool-call รอบแรกไปกับการ `search_files('*')` สำรวจเปล่า ๆ:

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

---

## `ola ask`

```
Usage: ola ask [options] <prompt> [files...]
       ola ask [options] -f <prompt-file> [files...]
```

### Tool พื้นฐาน 8 ตัว (มีเสมอ ไม่มีเงื่อนไข)

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

### Tool แบบมีเงื่อนไข (เปิดเมื่อ config ตรงเงื่อนไขเท่านั้น)

- **`run_command`** — เปิดอัตโนมัติเมื่อเจอ toolchain ที่รู้จักใน current directory (`go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`/`requirements.txt`/`setup.py`, `Makefile`) และไม่ได้ปิดด้วย `-V/--no-verify`
- **`web_search`** / **`web_fetch`** — `web_fetch` เปิดเสมอ, `web_search` เปิดเมื่อ config backend ใดบ้างหนึ่ง (ดู [Web search](#web-search--web-fetch))
- **`read_skill`** — เปิดเมื่อตั้ง `--skills-dir`/`OLA_SKILLS_DIR` (ดู [Skills](#skills-system))
- **`scp_copy`** — เปิดเมื่อตั้ง `--scp-hosts`/`OLA_SCP_HOSTS`
- **`api_request`** — เปิดเมื่อตั้ง `--api-endpoints`/`OLA_API_ENDPOINTS` หรือ `--api-allow-direct-url`

### กลไก Verify (เปิดอัตโนมัติ)

ถ้า current directory มี toolchain ที่รู้จัก ola จะเพิ่ม tool `run_command` ให้โมเดิลใช้ build/test เองระหว่างทาง **และ**ถ้าโมเดิลแก้ไฟล์ (`write_file`/`edit_file`) ในเซสชันนี้ ก่อนตอบจบ ola จะรัน build/test ของโปรเจกต์เองอีกครั้งอย่างอิสระ (ไม่เชื่อคำโมเดิลเปล่า ๆ) ถ้าไม่ผ่านจะป้อนผลลัพธ์กลับให้โมเดิลแก้ต่อ **สูงสุด 3 รอบ** ก่อนหยุดให้ผู้ใช้ตรวจสอบเอง — trigger เฉพาะเมื่อไฟล์ที่แก้เป็น source file ของ toolchain จริง ๆ (แก้ `.md`/`.txt` ไม่ trigger) ปิดทั้งหมดได้ด้วย `-V/--no-verify`

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
  -V, --no-verify       ปิดการ verify อัตโนมัติทั้งหมด
      --cmd-timeout <sec>       timeout ต่อ run_command หนึ่งครั้ง (default: 60)
      --ollama-search-key <k>  เปิด web_search ผ่าน Ollama hosted API
      --searxng-url <u>        เปิด web_search ผ่าน SearXNG (ชนะ Ollama key ถ้าตั้งคู่กัน)
      --no-web-search           ปิดทั้ง web_search และ web_fetch
      --skills-dir <list>       เปิด read_skill (comma-separated หลาย directory ได้)
      --scp-hosts <list>        เปิด scp_copy
      --scp-local-dir/-key/-timeout/-max-bytes
      --api-endpoints <list>    เปิด api_request
      --api-allow-direct-url / --api-allow-mutating / --api-timeout
  -h, --help             แสดงข้อความช่วยเหลือนี้
```

### ไฟล์แนบ (`[files...]`)

- `.jpg .jpeg .png .webp .gif` → อ่านและแนบเป็น base64 ใน field `images` ของ user message
- นามสกุลอื่นทั้งหมด → อ่านเป็นข้อความต่อท้ายเข้าไปใน prompt โดยตรง (คั่นด้วย separator เว้นแต่ใช้ `-r/--raw`)
- ไฟล์ที่ไม่พบ → แสดง warning แล้วข้าม ไม่หยุดโปรแกรม

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

### Tool เพิ่มเติม (นอกเหนือจาก 8 ตัวของ `ask`)

`add_tasks`, `mark_task_done`, `run_command` (ไม่มี allowlist), `self_review_requirements`, `report_complete` — รวมถึง `web_fetch`/`web_search`/`api_request`/`read_skill` แบบมีเงื่อนไขเหมือน `ask` ทุกประการ

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

- **ไม่มี binary allowlist สำหรับ `run_command`** — มันรันคำสั่ง shell ใด ๆ ที่โมเดิลขอได้ ใช้ให้เหมาะกับความไว้ใจที่มีต่อโมเดิล/งานที่ทำ
- **`ask_user` ต้องมี stdin เป็น terminal จริง** — ใช้กับ script/cron/pipe (non-interactive) ไม่ได้ ถ้าโมเดิลเรียกจะได้ error แทน (ดี สำหรับ `ola coding` ที่ตั้งใจรันแบบไม่มีคนเฝ้า เพราะมันจะเลือก assumption เองแทนการค้างรอ)
- **Tool-calling loop มีเพดาน** — `ola ask` สูงสุด 25 รอบ, `ola coding` สูงสุด 300 รอบ (`--max-iterations`) หรือ 3 ชม. (`--max-duration`) แล้วแต่อันไหนถึงก่อน
- **System prompt แก้จากภายนอกไม่ได้** — ไม่มี `-s/--system` อีกต่อไป ปรับพฤติกรรมได้ผ่าน flag/env ที่มีให้เท่านั้น (หรือแก้ constant ในซอร์สแล้ว build ใหม่)
- **`web_fetch` ไม่รัน JavaScript** — หน้าเว็บที่เป็น client-side-rendered SPA ล้วนจะได้ error ชัดเจน ไม่ใช่เนื้อหาว่างเปล่า
- **ไม่รองรับ auto-detect .gitignore** สำหรับ directory tree ที่แปะเข้า prompt แรก — ยกเว้นเฉพาะโฟลเดอร์ที่รู้จักแบบ hardcode (`.git`, `node_modules`, `vendor`, `target`, `.venv`, `__pycache__`, `dist`, `build`, `.idea`, `.terraform` ฯลฯ) อาจไม่ตรงกับทุกโปรเจกต์เป๊ะ
