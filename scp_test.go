package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// ─────────────────────────────────────────────────────────────────
// parseSCPHostEntry / resolveSCPConfig
// ─────────────────────────────────────────────────────────────────

func TestParseSCPHostEntryValid(t *testing.T) {
	cases := []struct {
		entry                               string
		alias, user, host, port, remoteRoot string
	}{
		{"backup=moo@10.0.0.5:2222/srv/backup", "backup", "moo", "10.0.0.5", "2222", "/srv/backup"},
		{"nas=moo@nas.local/mnt/data", "nas", "moo", "nas.local", defaultSSHPort, "/mnt/data"},
		{"  spaced = moo@host /root  ", "spaced", "moo", "host", defaultSSHPort, "/root"},
	}
	for _, c := range cases {
		h, err := parseSCPHostEntry(c.entry)
		if err != nil {
			t.Fatalf("parseSCPHostEntry(%q) unexpected error: %v", c.entry, err)
		}
		if h.Alias != c.alias || h.User != c.user || h.Host != c.host || h.Port != c.port || h.RemoteRoot != c.remoteRoot {
			t.Fatalf("parseSCPHostEntry(%q) = %+v, want alias=%s user=%s host=%s port=%s root=%s",
				c.entry, h, c.alias, c.user, c.host, c.port, c.remoteRoot)
		}
	}
}

func TestParseSCPHostEntryInvalid(t *testing.T) {
	bad := []string{
		"",
		"noequalsigns",             // no "="
		"alias=onlyonefield",       // "=" but no "/" -> no remote root
		"alias=missingatsign/root", // hostspec has no "@"
		"=moo@host/root",           // empty alias
		"alias=@host/root",         // empty user
		"alias=moo@/root",          // empty host
	}
	for _, entry := range bad {
		if _, err := parseSCPHostEntry(entry); err == nil {
			t.Errorf("parseSCPHostEntry(%q) expected an error, got none", entry)
		}
	}
}

func TestResolveSCPConfigDisabledByDefault(t *testing.T) {
	t.Setenv("OLA_SCP_HOSTS", "")
	cfg, warnings := resolveSCPConfig("", "", "", 0, 0)
	if cfg.enabled() {
		t.Fatal("expected scp_copy to be disabled with no OLA_SCP_HOSTS/--scp-hosts configured")
	}
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings for an empty config, got: %v", warnings)
	}
}

// TestResolveSCPConfigFlagOverridesEnv confirms the flag>env>default
// precedence used throughout ola (resolveSearchConfig, resolveSkillsDirs)
// applies identically here: an explicit --scp-hosts wins over
// OLA_SCP_HOSTS.
func TestResolveSCPConfigFlagOverridesEnv(t *testing.T) {
	t.Setenv("OLA_SCP_HOSTS", "envalias=moo@envhost/env/root")
	cfg, warnings := resolveSCPConfig("flagalias=moo@flaghost/flag/root", "", "", 0, 0)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got: %v", warnings)
	}
	if !cfg.enabled() {
		t.Fatal("expected scp_copy to be enabled")
	}
	if _, ok := cfg.Hosts["flagalias"]; !ok {
		t.Fatalf("expected the --scp-hosts flag value to win over OLA_SCP_HOSTS, got hosts: %+v", cfg.Hosts)
	}
	if _, ok := cfg.Hosts["envalias"]; ok {
		t.Fatal("expected the env-only alias to be ignored once the flag is set")
	}
}

// TestResolveSCPConfigSkipsBadEntryButKeepsOthers confirms one malformed
// OLA_SCP_HOSTS entry produces a warning and is skipped, without taking
// down every other configured host - the same non-fatal shape loadSkills
// uses for a bad skill directory.
func TestResolveSCPConfigSkipsBadEntryButKeepsOthers(t *testing.T) {
	cfg, warnings := resolveSCPConfig("good=moo@goodhost/root,bad-entry-no-root", "", "", 0, 0)
	if len(warnings) != 1 {
		t.Fatalf("expected exactly 1 warning for the malformed entry, got %d: %v", len(warnings), warnings)
	}
	if _, ok := cfg.Hosts["good"]; !ok {
		t.Fatalf("expected the well-formed entry to still be loaded, got hosts: %+v", cfg.Hosts)
	}
	if len(cfg.Hosts) != 1 {
		t.Fatalf("expected exactly 1 loaded host, got %d: %+v", len(cfg.Hosts), cfg.Hosts)
	}
}

// TestResolveSCPConfigWarnsOnDuplicateAlias confirms a second entry reusing
// an already-seen alias is rejected with a warning and the first
// definition wins, rather than silently overwriting it.
func TestResolveSCPConfigWarnsOnDuplicateAlias(t *testing.T) {
	cfg, warnings := resolveSCPConfig("dup=moo@first/root1,dup=moo@second/root2", "", "", 0, 0)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "ซ้ำ") {
		t.Fatalf("expected exactly 1 duplicate-alias warning, got: %v", warnings)
	}
	if cfg.Hosts["dup"].Host != "first" {
		t.Fatalf("expected the FIRST definition of a duplicate alias to win, got host: %s", cfg.Hosts["dup"].Host)
	}
}

func TestResolveSCPConfigDefaultsLocalRootToCwd(t *testing.T) {
	dir := t.TempDir()
	origWD, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(origWD)

	cfg, _ := resolveSCPConfig("alias=moo@host/root", "", "", 0, 0)
	// Resolve symlinks on both sides (macOS/some CI temp dirs are
	// themselves behind a symlink) so this comparison is robust.
	wantAbs, _ := filepath.EvalSymlinks(dir)
	gotAbs, _ := filepath.EvalSymlinks(cfg.LocalRoot)
	if gotAbs != wantAbs {
		t.Fatalf("expected LocalRoot to default to cwd (%s), got %s", wantAbs, gotAbs)
	}
}

func TestResolveSCPConfigTimeoutAndMaxBytesDefaults(t *testing.T) {
	cfg, _ := resolveSCPConfig("alias=moo@host/root", "", "", 0, 0)
	if cfg.Timeout != defaultSCPTimeoutSec*time.Second {
		t.Fatalf("expected default timeout %ds, got %s", defaultSCPTimeoutSec, cfg.Timeout)
	}
	if cfg.MaxBytes != defaultSCPMaxBytes {
		t.Fatalf("expected default max bytes %d, got %d", defaultSCPMaxBytes, cfg.MaxBytes)
	}
}

// ─────────────────────────────────────────────────────────────────
// remoteSandboxedPath
// ─────────────────────────────────────────────────────────────────

func TestRemoteSandboxedPathAllowsSubpath(t *testing.T) {
	got, err := remoteSandboxedPath("/srv/backup", "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/srv/backup/sub/dir/file.txt" {
		t.Fatalf("got %q", got)
	}
}

func TestRemoteSandboxedPathRejectsTraversal(t *testing.T) {
	cases := []string{"../etc/passwd", "../../root/.ssh/id_rsa", "sub/../../etc/passwd"}
	for _, rel := range cases {
		if _, err := remoteSandboxedPath("/srv/backup", rel); err == nil {
			t.Errorf("remoteSandboxedPath(%q) expected traversal to be rejected", rel)
		}
	}
}

func TestRemoteSandboxedPathRejectsEmpty(t *testing.T) {
	if _, err := remoteSandboxedPath("/srv/backup", ""); err == nil {
		t.Fatal("expected empty remote_path to be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────
// toolSCPCopy - validation paths that never touch the network/subprocess
// ─────────────────────────────────────────────────────────────────

func testSCPConfig(t *testing.T, localRoot string) scpConfig {
	t.Helper()
	return scpConfig{
		Hosts: map[string]scpHost{
			"backup": {Alias: "backup", User: "moo", Host: "testhost", Port: "22", RemoteRoot: "/"},
		},
		HostOrder: []string{"backup"},
		LocalRoot: localRoot,
		Timeout:   5 * time.Second,
		MaxBytes:  1 << 20,
	}
}

func TestToolSCPCopyDisabledWithEmptyConfig(t *testing.T) {
	if _, err := toolSCPCopy(map[string]interface{}{}, scpConfig{}); err == nil {
		t.Fatal("expected an error when scp_copy is not configured")
	}
}

func TestToolSCPCopyRejectsBadDirection(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "sideways", "remote_alias": "backup",
		"local_path": "f.txt", "remote_path": "f.txt", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected an invalid direction to be rejected")
	}
}

func TestToolSCPCopyRejectsUnknownAlias(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "not-configured",
		"local_path": "f.txt", "remote_path": "f.txt", "reason": "test",
	}
	_, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected an unknown remote_alias to be rejected")
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Fatalf("expected the error to list the allowed alias(es), got: %v", err)
	}
}

func TestToolSCPCopyRejectsLocalPathEscape(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "../../etc/passwd", "remote_path": "f.txt", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected a local_path escaping the sandbox to be rejected")
	}
}

func TestToolSCPCopyRejectsRemotePathEscape(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := testSCPConfig(t, dir)
	cfg.Hosts["backup"] = scpHost{Alias: "backup", User: "moo", Host: "testhost", Port: "22", RemoteRoot: "/srv/backup"}
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "f.txt", "remote_path": "../../etc/passwd", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected a remote_path escaping the alias's remote root to be rejected")
	}
}

func TestToolSCPCopyRejectsDirectoryUpload(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "adir"), 0755); err != nil {
		t.Fatal(err)
	}
	cfg := testSCPConfig(t, dir)
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "adir", "remote_path": "adir", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected uploading a directory to be rejected (scp_copy is single-file only)")
	}
}

func TestToolSCPCopyRejectsOversizedUpload(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("x", 2<<20) // 2MB, over the 1MB cap set in testSCPConfig
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), []byte(big), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := testSCPConfig(t, dir)
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "big.bin", "remote_path": "big.bin", "reason": "test",
	}
	_, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected an oversized upload to be rejected before ever touching the network")
	}
	if !strings.Contains(err.Error(), "เกินขีดจำกัด") {
		t.Fatalf("expected the size-cap error to explain itself, got: %v", err)
	}
}

func TestToolSCPCopyRejectsMissingLocalSourceOnUpload(t *testing.T) {
	cfg := testSCPConfig(t, t.TempDir())
	args := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "does-not-exist.txt", "remote_path": "f.txt", "reason": "test",
	}
	if _, err := toolSCPCopy(args, cfg); err == nil {
		t.Fatal("expected uploading a nonexistent local file to be rejected")
	}
}

// ─────────────────────────────────────────────────────────────────
// Subprocess-level tests using a fake `scp` binary on PATH, mirroring how
// coding_test.go's TestRunShellCommandTimeout/TestToolRunCommandExecutesAllowedCommand
// exercise real subprocess behavior rather than mocking exec.Command away.
// The fake binary treats any "user@host:remote/path" endpoint as
// $FAKE_SCP_REMOTE_ROOT/remote/path, which lets upload/download be
// exercised end-to-end (argv construction, timeout/process-group kill,
// exit-code handling) without a real SSH server or network access.
// ─────────────────────────────────────────────────────────────────

const fakeSCPScript = `#!/bin/sh
# Fake scp for ola's tests: strips the ssh-only flags scp_copy always
# passes (-q, -P <port>, -o <opt> x2, optional -i <key>), then treats
# whichever of the two remaining positional args looks like
# "user@host:path" as living under $FAKE_SCP_REMOTE_ROOT instead of a real
# remote host.
skip_next=0
src=""
dst=""
for a in "$@"; do
  if [ "$skip_next" = "1" ]; then skip_next=0; continue; fi
  case "$a" in
    -q) continue ;;
    -P) skip_next=1; continue ;;
    -o) skip_next=1; continue ;;
    -i) skip_next=1; continue ;;
    *)
      if [ -z "$src" ]; then src="$a"; else dst="$a"; fi
      ;;
  esac
done
resolve() {
  case "$1" in
    *@*:*) printf '%s' "$FAKE_SCP_REMOTE_ROOT/${1#*:}" ;;
    *) printf '%s' "$1" ;;
  esac
}
rsrc=$(resolve "$src")
rdst=$(resolve "$dst")
mkdir -p "$(dirname "$rdst")" || exit 1
cp "$rsrc" "$rdst"
`

const fakeSCPScriptTimeout = `#!/bin/sh
sleep 5
`

// installFakeSCP writes the given script as an executable "scp" and
// prepends its directory to PATH for the duration of the test, so
// exec.Command("scp", ...) inside runSCPCommand picks it up instead of any
// real scp installed on the machine running the test.
func installFakeSCP(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake scp shell script requires a POSIX shell")
	}
	dir := t.TempDir()
	scpPath := filepath.Join(dir, "scp")
	if err := os.WriteFile(scpPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+origPath)
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
}

func TestToolSCPCopyUploadDownloadRoundTrip(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	localDir := t.TempDir()
	remoteDir := t.TempDir() // stands in for the whole remote filesystem, rooted at "/"
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	const content = "สวัสดีจาก ola scp_copy test\n"
	if err := os.WriteFile(filepath.Join(localDir, "upload-me.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testSCPConfig(t, localDir)

	// Upload: local -> "remote" (really remoteDir on disk, via the fake binary)
	uploadArgs := map[string]interface{}{
		"direction": "upload", "remote_alias": "backup",
		"local_path": "upload-me.txt", "remote_path": "uploaded.txt", "reason": "ทดสอบ upload",
	}
	result, err := toolSCPCopy(uploadArgs, cfg)
	if err != nil {
		t.Fatalf("expected upload to succeed, got error: %v (result: %s)", err, result)
	}
	if !strings.Contains(result, "upload") {
		t.Fatalf("expected the success message to mention the direction, got: %s", result)
	}
	uploaded, err := os.ReadFile(filepath.Join(remoteDir, "uploaded.txt"))
	if err != nil {
		t.Fatalf("expected the file to land in the fake remote root: %v", err)
	}
	if string(uploaded) != content {
		t.Fatalf("expected uploaded content to match, got: %q", uploaded)
	}

	// Download: "remote" -> local, a different file this time.
	if err := os.WriteFile(filepath.Join(remoteDir, "on-remote.txt"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	downloadArgs := map[string]interface{}{
		"direction": "download", "remote_alias": "backup",
		"local_path": "downloaded.txt", "remote_path": "on-remote.txt", "reason": "ทดสอบ download",
	}
	result, err = toolSCPCopy(downloadArgs, cfg)
	if err != nil {
		t.Fatalf("expected download to succeed, got error: %v (result: %s)", err, result)
	}
	downloaded, err := os.ReadFile(filepath.Join(localDir, "downloaded.txt"))
	if err != nil {
		t.Fatalf("expected the downloaded file to land in the local sandbox: %v", err)
	}
	if string(downloaded) != content {
		t.Fatalf("expected downloaded content to match, got: %q", downloaded)
	}
}

// TestToolSCPCopyRejectsOversizedDownloadAfterTransfer confirms the
// post-transfer size check (the only option for downloads, since scp gives
// no way to know the remote file's size up front) actually deletes the
// oversized file rather than leaving it sitting in the sandbox.
func TestToolSCPCopyRejectsOversizedDownloadAfterTransfer(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	localDir := t.TempDir()
	remoteDir := t.TempDir()
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	big := strings.Repeat("y", 2<<20) // 2MB, over the 1MB cap
	if err := os.WriteFile(filepath.Join(remoteDir, "too-big.bin"), []byte(big), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := testSCPConfig(t, localDir)
	args := map[string]interface{}{
		"direction": "download", "remote_alias": "backup",
		"local_path": "too-big.bin", "remote_path": "too-big.bin", "reason": "test",
	}
	_, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected an oversized download to be rejected after transfer")
	}
	if _, statErr := os.Stat(filepath.Join(localDir, "too-big.bin")); !os.IsNotExist(statErr) {
		t.Fatal("expected the oversized downloaded file to be deleted, but it still exists")
	}
}

// TestToolSCPCopyPropagatesNonZeroExit confirms a failing transfer (fake
// scp exits non-zero because the source doesn't exist on the "remote"
// side) surfaces as an error with the exit code visible in the result,
// mirroring toolRunCommand's exit_code reporting.
func TestToolSCPCopyPropagatesNonZeroExit(t *testing.T) {
	installFakeSCP(t, fakeSCPScript)

	localDir := t.TempDir()
	remoteDir := t.TempDir() // left empty - nothing to download
	t.Setenv("FAKE_SCP_REMOTE_ROOT", remoteDir)

	cfg := testSCPConfig(t, localDir)
	args := map[string]interface{}{
		"direction": "download", "remote_alias": "backup",
		"local_path": "nope.txt", "remote_path": "nope.txt", "reason": "test",
	}
	result, err := toolSCPCopy(args, cfg)
	if err == nil {
		t.Fatal("expected a failing transfer to return an error")
	}
	if !strings.Contains(result, "exit_code=") {
		t.Fatalf("expected the result to report an exit_code, got: %s", result)
	}
}

func TestRunSCPCommandTimeout(t *testing.T) {
	installFakeSCP(t, fakeSCPScriptTimeout)

	_, exitCode, err := runSCPCommand([]string{"-q", "src", "dst"}, 200*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if exitCode != -1 {
		t.Fatalf("expected exitCode -1 on timeout, got %d", exitCode)
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("expected the error to mention timeout, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────
// formatSCPNotification
// ─────────────────────────────────────────────────────────────────

func TestFormatSCPNotificationIncludesBothSidesAndReason(t *testing.T) {
	got := formatSCPNotification("upload", "backup", "logs/app.log", "incoming/app.log", "ส่ง log ประจำวันไปสำรอง")
	if !strings.Contains(got, "UPLOAD") {
		t.Fatalf("expected the direction to be shown uppercased, got: %s", got)
	}
	for _, want := range []string{"logs/app.log", "backup", "incoming/app.log", "ส่ง log ประจำวันไปสำรอง"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected notification to contain %q, got: %s", want, got)
		}
	}
}

func TestFormatSCPNotificationHandlesEmptyReason(t *testing.T) {
	got := formatSCPNotification("download", "nas", "a.txt", "b.txt", "")
	if strings.Contains(got, " - ") {
		t.Fatalf("expected no dangling separator when reason is empty, got: %s", got)
	}
}
