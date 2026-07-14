// scp.go - optional "scp_copy" tool: copies a single file between the
// local sandbox and an operator-approved remote host over SSH, using the
// system `scp` binary (see 6.A in the design discussion this followed:
// shelling out to the system binary rather than adding a Go SSH/SFTP
// dependency, keeping ola's zero-Go-dependency philosophy - see search.go's
// header - intact; this is the one place ola depends on an external
// binary, the same way run_command depends on whatever toolchain binaries
// (go/npm/cargo/...) happen to be installed).
//
// This tool is opt-in like everything else that reaches outside the
// sandbox (run_command/web_search/web_fetch/read_skill - see coding.go/
// search.go/skills.go): unless OLA_SCP_HOSTS/--scp-hosts is actually
// configured, scp_copy is never added to the tool list and nothing in this
// file runs.
//
// Design principles (deliberately stricter than run_command/web_fetch,
// because this tool moves data across the network in both directions -
// upload is a genuine exfiltration channel if left unconstrained):
//
//  1. The remote user/host/port/root directory are NEVER supplied by the
//     model. They come exclusively from OLA_SCP_HOSTS/--scp-hosts, set by
//     the human running ola. The model can only pick a "remote_alias" name
//     out of that pre-approved list - the same "deterministic allowlist,
//     not model input" principle validateCommand's binary allowlist uses
//     for run_command, just applied to a name instead of a command.
//  2. Both sides are sandboxed by path, the same way read_file/write_file
//     are (sandboxedPathIn) - local_path can never escape the configured
//     local root, remote_path can never escape the remote root configured
//     for that specific alias.
//  3. Auth is exclusively via a pre-configured SSH key (ssh-agent, the
//     user's own ~/.ssh/config, or an explicit --scp-key/OLA_SCP_KEY
//     identity file) with BatchMode=yes (fail instead of prompting) and
//     StrictHostKeyChecking=yes (never silently trust an unknown/changed
//     host key). Nothing resembling a password is ever accepted as a tool
//     argument or read from the model.
//  4. No confirmation prompt (ask_user) before running - by design, so
//     scp_copy behaves like write_file/edit_file (immediate, no
//     human-in-the-loop pause) rather than like a "destructive, ask first"
//     action. The safety net here is entirely in what's ALLOWED (points
//     1-3 and the size/timeout caps below), not in a per-call confirm.
//  5. Every successful transfer is recorded into the session's change log
//     and pushed as its own ntfy.sh notification - more prominent than a
//     plain write_file, since data leaving the machine over the network is
//     more consequential than a local edit and deserves to be unmissable.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultSCPTimeoutSec bounds how long a single transfer may run before
	// ola kills it - file transfers legitimately take longer than a
	// build/test command, hence a higher default than run_command's.
	defaultSCPTimeoutSec = 120

	// defaultSCPMaxBytes caps the size of a single file scp_copy will move,
	// in either direction - same rationale as maxFetchDownloadBytes in
	// search.go: a multi-GB file must be rejected outright rather than
	// silently tying up the whole session.
	defaultSCPMaxBytes = 100 << 20 // 100MB

	defaultSSHPort = "22"
)

// scpHost is one operator-approved remote target: everything the model
// itself is never allowed to specify (user, host, port, and the remote
// directory scp_copy is sandboxed to for this alias).
type scpHost struct {
	Alias      string
	User       string
	Host       string
	Port       string
	RemoteRoot string // absolute path; the sandbox root on the remote side for this alias
}

// scpConfig is the resolved result of OLA_SCP_HOSTS/--scp-hosts plus the
// local-side sandbox root, SSH identity, timeout, and size cap. enabled()
// gates whether scp_copy is offered to the model at all, mirroring
// searchConfig.searchEnabled()/fetchEnabled() in search.go.
type scpConfig struct {
	Hosts     map[string]scpHost
	HostOrder []string // preserves config order, used for stable-ish error listings before sorting
	LocalRoot string    // absolute path; the sandbox root on the local side (default: cwd)
	KeyPath   string    // optional -i identity file; empty = rely on ssh-agent/~/.ssh/config
	Timeout   time.Duration
	MaxBytes  int64 // 0 disables the cap entirely; resolveSCPConfig never produces 0 unless explicitly forced
}

func (c scpConfig) enabled() bool { return len(c.Hosts) > 0 }

// aliasList renders the allowed alias names for error messages (e.g. "the
// model picked an alias that isn't configured") - sorted so the message is
// stable/testable rather than depending on map iteration order.
func (c scpConfig) aliasList() string {
	names := append([]string{}, c.HostOrder...)
	sort.Strings(names)
	if len(names) == 0 {
		return "(ไม่มี - scp_copy ปิดใช้งานอยู่)"
	}
	return strings.Join(names, ", ")
}

// resolveSCPConfig applies flag > env > default precedence, the same
// convention used throughout ola (see resolveSearchConfig in search.go,
// resolveSkillsDirs in skills.go). Parse errors for individual
// OLA_SCP_HOSTS entries are collected as warnings (that one alias is
// skipped) rather than being fatal - a typo in one entry shouldn't take
// down every other configured host, the same non-fatal-warning shape
// loadSkills uses for a bad skill directory.
func resolveSCPConfig(hostsFlag, localDirFlag, keyFlag string, timeoutSecFlag int, maxBytesFlag int64) (scpConfig, []string) {
	var warnings []string

	raw := hostsFlag
	if raw == "" {
		raw = os.Getenv("OLA_SCP_HOSTS")
	}
	hosts := map[string]scpHost{}
	var order []string
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		h, err := parseSCPHostEntry(entry)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("OLA_SCP_HOSTS: ข้าม entry %q (%v)", entry, err))
			continue
		}
		if _, dup := hosts[h.Alias]; dup {
			warnings = append(warnings, fmt.Sprintf("OLA_SCP_HOSTS: alias %q ซ้ำ - ใช้ตัวแรกที่เจอ", h.Alias))
			continue
		}
		hosts[h.Alias] = h
		order = append(order, h.Alias)
	}

	localRoot := strings.TrimSpace(localDirFlag)
	if localRoot == "" {
		localRoot = os.Getenv("OLA_SCP_LOCAL_DIR")
	}
	if localRoot == "" {
		if cwd, err := os.Getwd(); err == nil {
			localRoot = cwd
		}
	}
	if abs, err := filepath.Abs(localRoot); err == nil {
		localRoot = filepath.Clean(abs)
	}

	keyPath := strings.TrimSpace(keyFlag)
	if keyPath == "" {
		keyPath = strings.TrimSpace(os.Getenv("OLA_SCP_KEY"))
	}

	timeoutSec := timeoutSecFlag
	if timeoutSec <= 0 {
		timeoutSec = envInt("OLA_SCP_TIMEOUT_SEC", defaultSCPTimeoutSec)
	}

	maxBytes := maxBytesFlag
	if maxBytes <= 0 {
		maxBytes = int64(envInt("OLA_SCP_MAX_BYTES", defaultSCPMaxBytes))
	}

	return scpConfig{
		Hosts:     hosts,
		HostOrder: order,
		LocalRoot: localRoot,
		KeyPath:   keyPath,
		Timeout:   time.Duration(timeoutSec) * time.Second,
		MaxBytes:  maxBytes,
	}, warnings
}

// parseSCPHostEntry parses one "alias=user@host[:port]=/remote/root" entry
// from OLA_SCP_HOSTS/--scp-hosts. This is the ONLY place a remote
// user/host/port/root is ever set - see the package doc comment above.
// Example: "backup=moo@10.0.0.5:22=/srv/backup" or, using the default SSH
// port, "nas=moo@nas.local=/mnt/data".
func parseSCPHostEntry(entry string) (scpHost, error) {
	parts := strings.SplitN(entry, "=", 3)
	if len(parts) != 3 {
		return scpHost{}, fmt.Errorf(`รูปแบบต้องเป็น "alias=user@host[:port]=/remote/root"`)
	}
	alias := strings.TrimSpace(parts[0])
	hostspec := strings.TrimSpace(parts[1])
	root := strings.TrimSpace(parts[2])
	if alias == "" || hostspec == "" || root == "" {
		return scpHost{}, fmt.Errorf("alias/hostspec/remote-root ต้องไม่ว่างเปล่า")
	}
	if !strings.HasPrefix(root, "/") {
		return scpHost{}, fmt.Errorf("remote root %q ต้องเป็น absolute path (ขึ้นต้นด้วย /)", root)
	}

	userHost := hostspec
	port := defaultSSHPort
	// Naive "user@host:port" split. IPv6 literal hosts (which contain their
	// own colons) are out of scope for this feature - a realistic target
	// here is a home/lab server or NAS by hostname or IPv4, not an IPv6
	// literal - so this simple LastIndex approach is deliberately not
	// bullet-proofed against that case.
	if idx := strings.LastIndex(userHost, ":"); idx != -1 {
		if p, err := strconv.Atoi(userHost[idx+1:]); err == nil && p > 0 && p <= 65535 {
			port = userHost[idx+1:]
			userHost = userHost[:idx]
		}
	}
	atIdx := strings.Index(userHost, "@")
	if atIdx <= 0 || atIdx == len(userHost)-1 {
		return scpHost{}, fmt.Errorf(`hostspec %q ต้องเป็นรูปแบบ "user@host"`, hostspec)
	}
	user := userHost[:atIdx]
	host := userHost[atIdx+1:]

	return scpHost{
		Alias:      alias,
		User:       user,
		Host:       host,
		Port:       port,
		RemoteRoot: path.Clean(root),
	}, nil
}

// remoteSandboxedPath resolves rel against root and rejects anything (via
// absolute paths or "..") that would escape root - the same guard
// sandboxedPathIn (main.go) applies to the local side, just built on the
// "path" package instead of "path/filepath": the remote side is always
// reached over SSH and is conventionally a Unix-like filesystem regardless
// of whatever OS ola itself happens to be built for, so POSIX slash rules
// are the correct ones here even on a hypothetical non-Linux ola build.
func remoteSandboxedPath(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("remote_path ว่างเปล่า")
	}
	rootClean := path.Clean(root)
	joined := path.Clean(path.Join(rootClean, rel))
	if joined != rootClean {
		// Avoid a "//" prefix check when root is literally "/" (a valid,
		// if unusually permissive, config meaning "the whole remote
		// filesystem") - "/" + "/" would otherwise never match anything.
		prefix := rootClean
		if prefix != "/" {
			prefix += "/"
		}
		if !strings.HasPrefix(joined, prefix) {
			return "", fmt.Errorf("remote_path นอกขอบเขตที่อนุญาตสำหรับ alias นี้: %s", rel)
		}
	}
	return joined, nil
}

// ─────────────────────────────────────────────────────────────────
// Tool schema
// ─────────────────────────────────────────────────────────────────

var scpCopyTool = ollamaTool{
	Type: "function",
	Function: ollamaToolFunction{
		Name: "scp_copy",
		Description: "คัดลอกไฟล์หนึ่งไฟล์ระหว่างเครื่องนี้กับ remote host ที่ผู้ใช้ตั้งค่าอนุญาตไว้ล่วงหน้าเท่านั้น " +
			"(ผ่าน OLA_SCP_HOSTS/--scp-hosts) - เลือกได้แค่ remote_alias จากรายชื่อที่ตั้งไว้ ห้ามระบุ user/host/พาธเต็ม " +
			"ของฝั่ง remote เอง ใช้ SSH key ที่ config ไว้แล้วในเครื่อง (ssh-agent หรือ ~/.ssh/config หรือ --scp-key) " +
			"เท่านั้น ไม่รองรับและไม่รับ password ใดๆ local_path และ remote_path เป็น path สัมพัทธ์ภายใน sandbox " +
			"ที่อนุญาตของแต่ละฝั่งเท่านั้น (ออกนอกขอบเขตที่ตั้งค่าไว้ไม่ได้) เรียก tool นี้ได้ทันทีไม่ต้องขอ confirm จากผู้ใช้ก่อน",
		Parameters: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"direction": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"upload", "download"},
					"description": "upload: local_path -> remote host, download: remote host -> local_path",
				},
				"remote_alias": map[string]interface{}{
					"type":        "string",
					"description": "ชื่อ alias ของ remote host ที่ตั้งค่าไว้ล่วงหน้าเท่านั้น (ผิดชื่อจะได้ error พร้อมรายชื่อที่อนุญาตจริง)",
				},
				"local_path": map[string]interface{}{
					"type":        "string",
					"description": "path สัมพัทธ์ภายใต้ local sandbox (default: current directory, หรือ --scp-local-dir/OLA_SCP_LOCAL_DIR ถ้าตั้งไว้)",
				},
				"remote_path": map[string]interface{}{
					"type":        "string",
					"description": "path สัมพัทธ์ภายใต้ remote root ที่ตั้งค่าไว้สำหรับ alias นี้ใน OLA_SCP_HOSTS",
				},
				"reason": map[string]interface{}{
					"type":        "string",
					"description": "อธิบายสั้นๆ ว่าทำไมถึง copy ไฟล์นี้ - surfaced ให้ผู้ใช้เห็นตรงๆ ผ่าน notification/log เขียนสำหรับคนอ่าน",
				},
			},
			"required": []string{"direction", "remote_alias", "local_path", "remote_path", "reason"},
		},
	},
}

// ─────────────────────────────────────────────────────────────────
// Tool implementation
// ─────────────────────────────────────────────────────────────────

func toolSCPCopy(args map[string]interface{}, cfg scpConfig) (string, error) {
	if !cfg.enabled() {
		return "", fmt.Errorf("scp_copy ถูกปิดใช้งานอยู่ (ยังไม่ได้ตั้งค่า OLA_SCP_HOSTS/--scp-hosts)")
	}
	direction, _ := args["direction"].(string)
	alias, _ := args["remote_alias"].(string)
	localRel, _ := args["local_path"].(string)
	remoteRel, _ := args["remote_path"].(string)

	if direction != "upload" && direction != "download" {
		return "", fmt.Errorf(`direction ต้องเป็น "upload" หรือ "download" เท่านั้น`)
	}
	host, ok := cfg.Hosts[alias]
	if !ok {
		return "", fmt.Errorf("remote_alias %q ไม่อยู่ในรายชื่อที่อนุญาต (อนุญาตเฉพาะ: %s)", alias, cfg.aliasList())
	}

	localFull, err := sandboxedPathIn(cfg.LocalRoot, localRel)
	if err != nil {
		return "", err
	}
	remoteFull, err := remoteSandboxedPath(host.RemoteRoot, remoteRel)
	if err != nil {
		return "", err
	}
	remoteSpec := fmt.Sprintf("%s@%s:%s", host.User, host.Host, remoteFull)

	// Pre-flight size check for uploads: the local file's size is known
	// ahead of time, so a too-large source is rejected before ever
	// touching the network. Downloads can't be pre-checked this way (scp
	// doesn't report the remote file's size up front) - that direction is
	// checked AFTER the transfer completes, below.
	if direction == "upload" {
		info, statErr := os.Stat(localFull)
		if statErr != nil {
			return "", fmt.Errorf("อ่านไฟล์ต้นทาง %s ไม่ได้: %v", localRel, statErr)
		}
		if info.IsDir() {
			return "", fmt.Errorf("%s เป็น directory - scp_copy รองรับเฉพาะไฟล์เดี่ยว", localRel)
		}
		if cfg.MaxBytes > 0 && info.Size() > cfg.MaxBytes {
			return "", fmt.Errorf("ไฟล์ %s ขนาด %d bytes เกินขีดจำกัด %d bytes (OLA_SCP_MAX_BYTES)", localRel, info.Size(), cfg.MaxBytes)
		}
	}

	argv := []string{"-q", "-P", host.Port, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes"}
	if cfg.KeyPath != "" {
		argv = append(argv, "-i", cfg.KeyPath)
	}
	var src, dst string
	if direction == "upload" {
		src, dst = localFull, remoteSpec
	} else {
		src, dst = remoteSpec, localFull
	}
	argv = append(argv, src, dst)

	out, exitCode, err := runSCPCommand(argv, cfg.Timeout)
	if err != nil {
		return fmt.Sprintf("exit_code=%d\n%s", exitCode, out), err
	}
	if exitCode != 0 {
		return fmt.Sprintf("exit_code=%d\n%s", exitCode, out), fmt.Errorf("scp ล้มเหลว (exit_code=%d): %s", exitCode, strings.TrimSpace(out))
	}

	if direction == "download" {
		if info, statErr := os.Stat(localFull); statErr == nil {
			if cfg.MaxBytes > 0 && info.Size() > cfg.MaxBytes {
				_ = os.Remove(localFull)
				return "", fmt.Errorf("ไฟล์ที่ดาวน์โหลดมาขนาด %d bytes เกินขีดจำกัด %d bytes (OLA_SCP_MAX_BYTES) - ลบไฟล์ทิ้งแล้ว", info.Size(), cfg.MaxBytes)
			}
		}
	}

	return fmt.Sprintf("สำเร็จ: %s %s <-> %s:%s (alias=%s)", direction, localRel, host.Host, remoteRel, alias), nil
}

// runSCPCommand executes the system `scp` binary directly via an argv
// slice - NEVER through "sh -c" the way run_command's runShellCommand
// chains build/test commands - so nothing in host/path/reason can be
// interpreted as shell syntax: there is no chaining operator, no
// metacharacter expansion, nothing for a crafted path or alias to inject
// into. Bounded by timeout and killable as a whole process group, reusing
// the exact same mechanism runShellCommand uses (setupProcessGroup/
// killProcessGroup - see proc_linux.go/proc_other.go) so a hung transfer
// (e.g. a flaky link) can't outlive its timeout.
func runSCPCommand(argv []string, timeout time.Duration) (output string, exitCode int, err error) {
	c := exec.Command("scp", argv...)
	c.Env = os.Environ()
	setupProcessGroup(c)

	done := make(chan error, 1)
	var outBuf strings.Builder
	c.Stdout = &outBuf
	c.Stderr = &outBuf

	if startErr := c.Start(); startErr != nil {
		return "", -1, fmt.Errorf("เรียก scp ไม่สำเร็จ (ไม่พบ scp binary ในเครื่อง หรือรันไม่ได้): %v", startErr)
	}
	go func() { done <- c.Wait() }()

	select {
	case waitErr := <-done:
		out := outBuf.String()
		if waitErr != nil {
			if exitErr, ok := waitErr.(*exec.ExitError); ok {
				return out, exitErr.ExitCode(), nil
			}
			return out, -1, waitErr
		}
		return out, 0, nil
	case <-time.After(timeout):
		killProcessGroup(c)
		<-done // reap the now-killed process so it doesn't linger as a zombie
		out := outBuf.String()
		return out, -1, fmt.Errorf("timeout: scp ใช้เวลาเกิน %s", timeout)
	}
}

// formatSCPNotification builds the "what happened" line for a successful
// scp_copy call - same shape/rationale as formatFileChangeNotification
// (main.go) for write_file/edit_file, surfaced directly to the human (e.g.
// in a push notification), so it names the actual transfer rather than
// just "done". Kept as its own function (rather than reusing
// formatFileChangeNotification directly) since scp_copy's notification
// needs to show BOTH sides of the transfer (local path AND remote
// alias/path), not just a single "path".
func formatSCPNotification(direction, alias, localPath, remotePath, reason string) string {
	base := fmt.Sprintf("[SCP:%s] %s <-> %s:%s", strings.ToUpper(direction), localPath, alias, remotePath)
	if reason = strings.TrimSpace(reason); reason != "" {
		base += " - " + reason
	}
	return truncateWords(base, maxNotificationWords)
}
