package vcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tomasz-tomczyk/crit/internal/diff"
)

// PerforceHaveRef names the revisions last synced into the selected client.
const PerforceHaveRef = "#have"

var errP4PendingOnly = errors.New("Perforce supports pending changelists in the current workspace")

type p4File struct {
	FileChange
	depotPath string
	baseSpec  string
}

// P4VCS is a read-only view of a single pending Perforce changelist.
type P4VCS struct {
	executable   string
	dir          string // invocation directory, so p4 still discovers the same P4CONFIG
	root         string
	server       string
	client       string
	user         string
	change       string
	message      string
	externalDiff bool

	mu    sync.RWMutex
	files map[string]p4File
}

type p4Info struct {
	ClientName    string `json:"clientName"`
	ClientRoot    string `json:"clientRoot"`
	ServerAddress string `json:"serverAddress"`
	UserName      string `json:"userName"`
}

type p4Change struct {
	Client      string
	Status      string
	Description string
}

type p4Opened struct {
	ClientFile string `json:"clientFile"`
	DepotFile  string `json:"depotFile"`
	HaveRev    string `json:"haveRev"`
	MovedFile  string `json:"movedFile"`
	Action     string `json:"action"`
	Type       string `json:"type"`
}

type p4Message struct {
	Data     string `json:"data"`
	Severity int    `json:"severity"`
	Generic  int    `json:"generic"`
}

func NewP4VCS(dir, change string) (*P4VCS, error) {
	if change == "" {
		change = "default"
	}
	if change != "default" {
		number, err := strconv.ParseUint(change, 10, 32)
		if err != nil || number == 0 {
			return nil, fmt.Errorf("invalid Perforce changelist %q: expected a positive number or default", change)
		}
		change = strconv.FormatUint(number, 10)
	}
	executable, err := exec.LookPath("p4")
	if err != nil {
		return nil, fmt.Errorf("Perforce CLI p4 is required: %w", err)
	}
	p := &P4VCS{executable: executable, dir: dir, change: change}
	ctx := context.Background()
	info, err := p4Records[p4Info](ctx, p, "info")
	if err != nil {
		return nil, err
	}
	if len(info) != 1 || info[0].ClientRoot == "" || info[0].ClientName == "" ||
		info[0].ClientName == "*unknown*" || info[0].ServerAddress == "" {
		return nil, errors.New("p4 info did not identify a Perforce workspace; check P4CONFIG or P4CLIENT")
	}
	p.root = filepath.Clean(info[0].ClientRoot)
	p.server, p.client, p.user = info[0].ServerAddress, info[0].ClientName, info[0].UserName
	if rel, err := filepath.Rel(p.root, dir); err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("run Crit inside the Perforce workspace %q", p.root)
	}
	if change != "default" {
		spec, err := p4Records[p4Change](ctx, p, "change", "-o", change)
		if err != nil {
			return nil, err
		}
		if len(spec) != 1 || spec[0].Status != "pending" || !strings.EqualFold(spec[0].Client, p.client) {
			return nil, fmt.Errorf("changelist %s is not pending in workspace %q", change, p.client)
		}
		p.message = strings.TrimSpace(spec[0].Description)
	}
	diffSetting, err := p.run(ctx, "set", "-q", "P4DIFF")
	if err != nil {
		return nil, err
	}
	p.externalDiff = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(diffSetting)), "P4DIFF=")) != ""
	return p, nil
}

func (p *P4VCS) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if p.client != "" {
		args = append([]string{"-c", p.client, "-u", p.user}, args...)
	}
	cmd := exec.CommandContext(ctx, p.executable, args...)
	cmd.Dir = p.dir
	// P4CONFIG takes precedence over these settings. Detect a diff command
	// configured there and compute the text diff locally in that case.
	cmd.Env = append(cmd.Environ(), "P4DIFF=", "DIFF=")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("p4 %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(string(out))
		}
		return out, fmt.Errorf("p4 %s: %w: %s", strings.Join(args, " "), err, detail)
	}
	return out, nil
}

func p4Records[T any](ctx context.Context, p *P4VCS, args ...string) ([]T, error) {
	out, runErr := p.run(ctx, append([]string{"-ztag", "-Mj"}, args...)...)
	decoder := json.NewDecoder(bytes.NewReader(out))
	var rows []T
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			return rows, runErr
		} else if err != nil {
			return nil, fmt.Errorf("decoding p4 %s output: %w", args[0], err)
		}
		var message p4Message
		if err := json.Unmarshal(raw, &message); err != nil {
			return nil, err
		}
		if message.Severity != 0 {
			// EV_EMPTY for an otherwise valid workspace with no opened files.
			if args[0] == "opened" && message.Generic == 17 {
				continue
			}
			return nil, fmt.Errorf("p4 %s: %s", args[0], strings.TrimSpace(message.Data))
		}
		var row T
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
}

var p4Unescape = strings.NewReplacer("%23", "#", "%40", "@", "%2A", "*", "%25", "%")

func (p *P4VCS) relativePath(clientFile string) (string, error) {
	prefix := "//" + p.client + "/"
	if len(clientFile) <= len(prefix) || !strings.EqualFold(clientFile[:len(prefix)], prefix) {
		return "", fmt.Errorf("invalid Perforce client path %q", clientFile)
	}
	rel := filepath.FromSlash(p4Unescape.Replace(clientFile[len(prefix):]))
	if !filepath.IsLocal(rel) || filepath.Clean(rel) != rel || rel == "." {
		return "", fmt.Errorf("invalid workspace-relative path %q", clientFile)
	}
	return filepath.ToSlash(rel), nil
}

func p4MetadataOnly(fileType, path string) bool {
	switch strings.SplitN(fileType, "+", 2)[0] {
	case "binary", "ubinary", "xbinary", "apple", "resource", "symlink", "utf16":
		return true
	}
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".uasset" || ext == ".umap"
}

func (p *P4VCS) scan() ([]FileChange, map[string]p4File, error) {
	rows, err := p4Records[p4Opened](context.Background(), p, "opened", "-c", p.change)
	if err != nil {
		return nil, nil, err
	}
	return p.mapOpenedFiles(rows)
}

func (p *P4VCS) mapOpenedFiles(rows []p4Opened) ([]FileChange, map[string]p4File, error) {
	byDepot := make(map[string]p4Opened, len(rows))
	for _, row := range rows {
		byDepot[row.DepotFile] = row
	}
	files := make(map[string]p4File, len(rows))
	changes := make([]FileChange, 0, len(rows))
	for _, row := range rows {
		peer, paired := byDepot[row.MovedFile]
		if row.Action == "move/delete" && paired && peer.Action == "move/add" {
			continue
		}
		entry, err := p.mapOpenedFile(row, peer, paired && peer.Action == "move/delete")
		if err != nil {
			return nil, nil, err
		}
		files[entry.Path] = entry
		if entry.OldPath != "" {
			files[entry.OldPath] = entry
		}
		changes = append(changes, entry.FileChange)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, files, nil
}

func (p *P4VCS) mapOpenedFile(row, peer p4Opened, hasMoveSource bool) (p4File, error) {
	path, err := p.relativePath(row.ClientFile)
	if err != nil {
		return p4File{}, err
	}
	fc := FileChange{Path: path, Status: "modified", Binary: p4MetadataOnly(row.Type, path)}
	base := row
	switch row.Action {
	case "move/add":
		fc.Status = "added"
		if hasMoveSource {
			fc.OldPath, err = p.relativePath(peer.ClientFile)
			if err != nil {
				return p4File{}, err
			}
			fc.Status, base = "renamed", peer
			fc.Binary = fc.Binary || p4MetadataOnly(peer.Type, fc.OldPath)
		}
	case "add", "branch":
		fc.Status = "added"
	case "delete", "move/delete":
		fc.Status = "deleted"
	case "integrate":
		if row.HaveRev == "" || row.HaveRev == "none" {
			fc.Status = "added"
		}
	}
	entry := p4File{FileChange: fc, depotPath: row.DepotFile}
	if fc.Status != "added" {
		entry.baseSpec = base.DepotFile + PerforceHaveRef
		if rev, err := strconv.Atoi(base.HaveRev); err == nil && rev > 0 {
			entry.baseSpec = base.DepotFile + "#" + base.HaveRev
		}
	}
	return entry, nil
}

func (p *P4VCS) ChangedFilesOnDefaultInDir(_ string) ([]FileChange, error) {
	changes, files, err := p.scan()
	if err == nil {
		p.mu.Lock()
		p.files = files
		p.mu.Unlock()
	}
	return changes, err
}

func (p *P4VCS) ChangedFilesFromBaseInDir(baseRef, dir string) ([]FileChange, error) {
	if baseRef != "" && baseRef != PerforceHaveRef {
		return nil, errP4PendingOnly
	}
	return p.ChangedFilesOnDefaultInDir(dir)
}

func (p *P4VCS) file(path string) (p4File, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	file, ok := p.files[path]
	return file, ok
}

func (p *P4VCS) FileDiffUnified(path, baseRef, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	return p.FileDiffUnifiedCtx(context.Background(), path, baseRef, dir, ignoreWhitespace)
}

func (p *P4VCS) FileDiffUnifiedCtx(ctx context.Context, path, baseRef, _ string, ignoreWhitespace bool) ([]DiffHunk, error) {
	if baseRef != "" && baseRef != PerforceHaveRef {
		return nil, errP4PendingOnly
	}
	f, ok := p.file(path)
	if !ok || f.Binary {
		return nil, nil
	}
	switch f.Status {
	case "added":
		data, err := os.ReadFile(filepath.Join(p.root, filepath.FromSlash(path)))
		return FileDiffUnifiedNewFile(p4Text(data)), err
	case "deleted":
		data, err := p.run(ctx, "print", "-q", f.baseSpec)
		return p4DeletionHunks(p4Text(data)), err
	}
	if p.externalDiff {
		base, err := p.run(ctx, "print", "-q", f.baseSpec)
		if err != nil {
			return nil, err
		}
		current, err := os.ReadFile(filepath.Join(p.root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		return p4LocalDiff(p4Text(base), p4Text(current), ignoreWhitespace), nil
	}
	mode := "-du"
	if ignoreWhitespace {
		mode = "-dwu"
	}
	out, err := p.run(ctx, "diff", mode, f.depotPath)
	if err != nil {
		return nil, err
	}
	return ParseUnifiedDiff(p4Text(out)), nil
}

func p4Text(data []byte) string { return strings.ReplaceAll(string(data), "\r\n", "\n") }

func p4LocalDiff(base, current string, ignoreWhitespace bool) []DiffHunk {
	base = strings.TrimSuffix(base, "\n")
	current = strings.TrimSuffix(current, "\n")
	if base == current {
		return nil
	}
	if !ignoreWhitespace {
		return diff.DiffEntriesToHunks(diff.ComputeLineDiff(base, current))
	}
	oldLines, newLines := diff.SplitLines(base), diff.SplitLines(current)
	normalize := func(lines []string) string {
		normalized := make([]string, len(lines))
		for i, line := range lines {
			normalized[i] = "|" + strings.Join(strings.Fields(line), "")
		}
		return strings.Join(normalized, "\n")
	}
	entries := diff.ComputeLineDiff(normalize(oldLines), normalize(newLines))
	for i := range entries {
		entry := &entries[i]
		if entry.NewLine != 0 {
			entry.Text = newLines[entry.NewLine-1]
		} else {
			entry.Text = oldLines[entry.OldLine-1]
		}
	}
	return diff.DiffEntriesToHunks(entries)
}

func p4DeletionHunks(content string) []DiffHunk {
	hunks := FileDiffUnifiedNewFile(content)
	for i := range hunks {
		h := &hunks[i]
		h.OldStart, h.OldCount, h.NewStart, h.NewCount = 1, h.NewCount, 0, 0
		h.Header = fmt.Sprintf("@@ -1,%d +0,0 @@", h.OldCount)
		for j := range h.Lines {
			line := &h.Lines[j]
			line.Type, line.OldNum, line.NewNum = "del", line.NewNum, 0
		}
	}
	return hunks
}

func (p *P4VCS) FileContentAtRef(path, ref, _ string) (string, error) {
	if ref != PerforceHaveRef {
		return "", errP4PendingOnly
	}
	f, ok := p.file(path)
	if !ok || f.Binary || f.baseSpec == "" {
		return "", nil
	}
	data, err := p.run(context.Background(), "print", "-q", f.baseSpec)
	return p4Text(data), err
}

func (p *P4VCS) WorkingTreeFingerprint() string {
	changes, files, err := p.scan()
	if err != nil {
		return ""
	}
	hash := sha256.New()
	for _, change := range changes {
		f := files[change.Path]
		fmt.Fprintf(hash, "%q %q %q %q\n", f.Path, f.OldPath, f.Status, f.baseSpec)
		if info, err := os.Stat(filepath.Join(p.root, filepath.FromSlash(f.Path))); err == nil {
			fmt.Fprintf(hash, "%d %d\n", info.Size(), info.ModTime().UnixNano())
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (p *P4VCS) SessionKeyArgs() []string {
	return []string{"p4", p.server, p.client, p.user, p.change}
}

func (p *P4VCS) ReviewArgs() []string { return []string{"--vcs", "p4", "--change", p.change} }

func (p *P4VCS) Name() string              { return "p4" }
func (p *P4VCS) RepoRoot() (string, error) { return p.root, nil }
func (p *P4VCS) CurrentBranch() string     { return p.client + " / CL " + p.change }
func (p *P4VCS) DefaultBranch() string     { return PerforceHaveRef }
func (p *P4VCS) DefaultBaseRef() string    { return PerforceHaveRef }
func (p *P4VCS) UserName() string          { return p.user }
func (p *P4VCS) HasStagingArea() bool      { return false }
func (p *P4VCS) SkipDirNames() []string    { return []string{".git"} }

func (p *P4VCS) MergeBase(ref string) (string, error) {
	if ref == PerforceHaveRef {
		return ref, nil
	}
	return "", errP4PendingOnly
}

func (p *P4VCS) CommitLog(_, _, _ string) ([]CommitInfo, error) {
	if p.message == "" {
		return nil, nil
	}
	return []CommitInfo{{Message: p.message}}, nil
}

func (p *P4VCS) FileStatusInRepo(path, _, _ string) string {
	f, _ := p.file(path)
	return f.Status
}

func (p *P4VCS) ChangedFilesScoped(scope, baseRef string) ([]FileChange, error) {
	if scope != "" && scope != "all" {
		return nil, errP4PendingOnly
	}
	return p.ChangedFilesFromBaseInDir(baseRef, p.root)
}

func (p *P4VCS) FileDiffScoped(path, scope, baseRef, dir string, ignoreWhitespace bool) ([]DiffHunk, error) {
	if scope != "" && scope != "all" {
		return nil, errP4PendingOnly
	}
	return p.FileDiffUnified(path, baseRef, dir, ignoreWhitespace)
}

func (p *P4VCS) FileDiffUnifiedNewFile(path string) ([]DiffHunk, error) {
	return p.FileDiffUnified(path, PerforceHaveRef, p.root, false)
}

// Changelist reviews have no Git index, branch history, or commit objects.
func (p *P4VCS) SetDefaultBranchOverride(string)             {}
func (p *P4VCS) GetDefaultBranchOverride() string            { return "" }
func (p *P4VCS) HasObject(string, string) bool               { return false }
func (p *P4VCS) UntrackedFiles(string) ([]FileChange, error) { return nil, nil }
func (p *P4VCS) RemoteBranches(string) ([]string, error)     { return nil, nil }
func (p *P4VCS) AllTrackedFiles(string) ([]string, error)    { return nil, errP4PendingOnly }
func (p *P4VCS) DiffNumstat(string, string) (map[string]NumstatEntry, error) {
	return nil, nil // file counts are populated from hunks as files load
}

func (p *P4VCS) MergeBaseOf(string, string, string) (string, error) {
	return "", errP4PendingOnly
}

func (p *P4VCS) ChangedFilesForCommit(string, string) ([]FileChange, error) {
	return nil, errP4PendingOnly
}

func (p *P4VCS) FileDiffForCommit(string, string, string, bool) ([]DiffHunk, error) {
	return nil, errP4PendingOnly
}

func (p *P4VCS) DiffNumstatBetweenSHAs(string, string, string) (map[string]NumstatEntry, error) {
	return nil, errP4PendingOnly
}

func (p *P4VCS) ChangedFilesBetweenSHAs(string, string, string) ([]FileChange, error) {
	return nil, errP4PendingOnly
}

func (p *P4VCS) FileDiffBetweenSHAs(string, string, string, string, string, bool) ([]DiffHunk, error) {
	return nil, errP4PendingOnly
}

func (p *P4VCS) ReadFileAtSHA(string, string, string) ([]byte, error) {
	return nil, errP4PendingOnly
}

var _ VCS = (*P4VCS)(nil)
