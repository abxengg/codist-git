// Package git reads a repository through the git command. Everything Codist needs from git goes
// through Repo, so a pure-Go backend can replace the command where git is not installed.
package git

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Repo is one repository on disk. Use it through a pointer: it keeps one git process open for reading files.
type Repo struct {
	Root string

	mu  sync.Mutex
	cat *batch // one "git cat-file --batch" for FileAt, started on first use: a process per file is far slower
}

// Close stops the git process FileAt keeps open. The Repo can still be used; the process starts again when needed.
func (r *Repo) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cat != nil {
		r.cat.close()
		r.cat = nil
	}
}

// Commit is one commit, newest first in lists.
type Commit struct {
	SHA     string    `json:"sha"`
	Short   string    `json:"short"`
	Author  string    `json:"author"`
	Time    time.Time `json:"time"`
	Subject string    `json:"subject"`
	Body    string    `json:"body"`
	Parents []string  `json:"parents"`
}

// FileChange is one file a commit changed, with the new-side line numbers it added or changed.
type FileChange struct {
	Path    string `json:"path"`
	OldPath string `json:"oldPath,omitempty"`
	Status  string `json:"status"` // added, modified, deleted, renamed
	Added   []int  `json:"added"`
	Patch   string `json:"patch"`
}

// Open finds the repository that contains dir.
func Open(dir string) (*Repo, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		logger().Debug("not a repository", "dir", dir, "err", err)
		return nil, fmt.Errorf("%s is not inside a git repository", dir)
	}
	return &Repo{Root: filepath.Clean(strings.TrimSpace(string(out)))}, nil
}

// Available says whether the git command can be run.
func Available() bool {
	_, err := exec.LookPath("git")
	return err == nil
}

func (r *Repo) run(args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", r.Root, "-c", "core.quotepath=off"}, args...)...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	start := time.Now()
	if err := cmd.Run(); err != nil {
		// Debug, not warn: many callers ask for things that may not exist, such as a file at an old commit.
		logger().Debug("git failed", "args", strings.Join(args, " "), "stderr", strings.TrimSpace(errOut.String()), "took", time.Since(start))
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errOut.String()))
	}
	logger().Debug("git", "args", strings.Join(args, " "), "bytes", out.Len(), "took", time.Since(start))
	return out.String(), nil
}

// Head is the commit checked out, or "" in an empty repository.
func (r *Repo) Head() string {
	if sha := r.headFromFiles(); sha != "" {
		return sha
	}
	out, err := r.run("rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// headFromFiles reads HEAD from the repository's own files, which costs no git process: Head is asked once for
// every commit reviewed. It answers "" for anything but the plain case (a .git folder, HEAD naming a branch whose
// ref is a loose file, or HEAD holding a commit id), and the git command is asked instead.
func (r *Repo) headFromFiles() string {
	dir := filepath.Join(r.Root, ".git")
	raw, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(raw))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		if !filepath.IsLocal(filepath.FromSlash(ref)) {
			return ""
		}
		if raw, err = os.ReadFile(filepath.Join(dir, filepath.FromSlash(ref))); err != nil {
			return "" // a packed ref, or a branch with no commit yet
		}
		head = strings.TrimSpace(string(raw))
	}
	if fullSHA.MatchString(head) {
		return head
	}
	return ""
}

// Branch is the checked-out branch name, or "detached".
func (r *Repo) Branch() string {
	out, err := r.run("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || strings.TrimSpace(out) == "HEAD" {
		return "detached"
	}
	return strings.TrimSpace(out)
}

const sep, end = "\x1f", "\x1e"

// unsafe says a revision (a commit id, a branch name) must not be handed to git: one that starts with a dash would
// be read as an option, and one with a line break would be read as a second request by cat-file. Revisions can come
// from outside, such as a web request, so every function that takes one checks it first.
func unsafe(revs ...string) bool {
	for _, r := range revs {
		if strings.HasPrefix(r, "-") || strings.ContainsAny(r, "\n\x00") {
			logger().Warn("revision refused", "rev", r)
			return true
		}
	}
	return false
}

var errUnsafe = fmt.Errorf("not a revision")

// Log lists up to limit commits reachable from HEAD, newest first.
func (r *Repo) Log(limit int) ([]Commit, error) { return r.LogRef("HEAD", limit) }

// LogRef lists up to limit commits reachable from ref, newest first.
func (r *Repo) LogRef(ref string, limit int) ([]Commit, error) {
	if unsafe(ref) {
		return nil, errUnsafe
	}
	out, err := r.run("log", "-n", strconv.Itoa(limit), "--format=%H"+sep+"%an"+sep+"%at"+sep+"%P"+sep+"%s"+sep+"%b"+end, ref, "--")
	if err != nil {
		return nil, err
	}
	var cs []Commit
	for _, rec := range strings.Split(out, end) {
		f := strings.Split(strings.TrimLeft(rec, "\n"), sep)
		if len(f) < 6 {
			continue
		}
		at, _ := strconv.ParseInt(f[2], 10, 64)
		cs = append(cs, Commit{SHA: f[0], Short: f[0][:min(7, len(f[0]))], Author: f[1], Time: time.Unix(at, 0),
			Parents: strings.Fields(f[3]), Subject: f[4], Body: strings.TrimSpace(f[5])})
	}
	return cs, nil
}

// Get reads one commit.
func (r *Repo) Get(sha string) (Commit, error) {
	if unsafe(sha) {
		return Commit{}, errUnsafe
	}
	out, err := r.run("log", "-n", "1", "--format="+commitFormat, sha, "--")
	if err != nil {
		return Commit{}, err
	}
	return parseCommit(out, sha)
}

const commitFormat = "%H" + sep + "%an" + sep + "%at" + sep + "%P" + sep + "%s" + sep + "%b" + end

func parseCommit(out, sha string) (Commit, error) {
	f := strings.Split(strings.TrimSuffix(strings.TrimSpace(out), end), sep)
	if len(f) < 6 {
		return Commit{}, fmt.Errorf("no commit %s", sha)
	}
	at, _ := strconv.ParseInt(f[2], 10, 64)
	return Commit{SHA: f[0], Short: f[0][:7], Author: f[1], Time: time.Unix(at, 0), Parents: strings.Fields(f[3]),
		Subject: f[4], Body: strings.TrimSpace(f[5])}, nil
}

// Show reads a commit and what it changed in one git call, for callers that need both. A merge commit's changes
// are what the person merging wrote by hand: the difference between the merge as committed and what git makes of
// the two sides on its own. A merge without conflicts changed nothing by hand.
func (r *Repo) Show(sha string) (Commit, []FileChange, error) {
	if unsafe(sha) {
		return Commit{}, nil, errUnsafe
	}
	out, err := r.run("show", "--format="+commitFormat, "--no-color", "-M", "-U3", "--no-ext-diff", sha, "--")
	if err != nil {
		return Commit{}, nil, err
	}
	head, diff, _ := strings.Cut(out, end)
	c, err := parseCommit(head, sha)
	if err != nil {
		return Commit{}, nil, err
	}
	if len(c.Parents) > 1 {
		diff, err = r.run("show", "--format=", "--no-color", "-M", "-U3", "--no-ext-diff", "--remerge-diff", c.SHA, "--")
		if err != nil { // a git older than 2.36 has no --remerge-diff: the merge is read as changing nothing
			logger().Debug("merge not read", "sha", c.Short, "err", err)
			return c, nil, nil
		}
	}
	return c, parseChanges(diff), nil
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// Changes lists what a commit changed against its first parent, or against nothing for the first commit.
func (r *Repo) Changes(sha string) ([]FileChange, error) {
	if unsafe(sha) {
		return nil, errUnsafe
	}
	_, files, err := r.Show(sha)
	return files, err
}

// parseChanges reads the output of git show or git diff into one FileChange for each file.
func parseChanges(out string) []FileChange {
	var files []FileChange
	var cur *FileChange
	var body strings.Builder
	newLine := 0
	flush := func() {
		if cur != nil {
			cur.Patch = body.String()
			files = append(files, *cur)
		}
		body.Reset()
	}
	inHunk := false // --- and +++ are headers only before a file's first hunk; after it they are changed lines
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &FileChange{Status: "modified"}
			inHunk = false
			continue
		case cur == nil:
			continue
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			if m := hunkRe.FindStringSubmatch(line); m != nil {
				newLine, _ = strconv.Atoi(m[1])
			}
			body.WriteString(line + "\n")
		case inHunk && strings.HasPrefix(line, "+"):
			cur.Added = append(cur.Added, newLine)
			newLine++
			body.WriteString(line + "\n")
		case inHunk && strings.HasPrefix(line, "-"):
			body.WriteString(line + "\n")
		case inHunk && strings.HasPrefix(line, " "):
			newLine++
			body.WriteString(line + "\n")
		case inHunk && strings.HasPrefix(line, `\`):
			body.WriteString(line + "\n")
		case inHunk:
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "deleted"
		case strings.HasPrefix(line, "rename from "):
			cur.OldPath, cur.Status = strings.TrimPrefix(line, "rename from "), "renamed"
		case strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "--- "):
			// git ends a name that has spaces with a tab
			if p := strings.TrimRight(strings.TrimPrefix(line, "--- "), "\t"); p != "/dev/null" && cur.OldPath == "" {
				cur.OldPath = strings.TrimPrefix(p, "a/")
			}
		case strings.HasPrefix(line, "+++ "):
			if p := strings.TrimRight(strings.TrimPrefix(line, "+++ "), "\t"); p != "/dev/null" {
				cur.Path = strings.TrimPrefix(p, "b/")
			} else {
				cur.Path = cur.OldPath
			}
		}
	}
	flush()
	return files
}

// FileAt reads a file as it was at a commit.
func (r *Repo) FileAt(sha, path string) ([]byte, bool) {
	if unsafe(sha) || strings.ContainsAny(path, "\n\x00") {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for try := 0; try < 2; try++ { // once more with a new process if the old one has stopped
		if r.cat == nil {
			b, err := r.newBatch()
			if err != nil {
				logger().Warn("cat-file not started", "err", err)
				return nil, false
			}
			r.cat = b
		}
		src, ok, err := r.cat.get(sha + ":" + path)
		if err == nil {
			return src, ok
		}
		logger().Debug("cat-file stopped; starting it again", "err", err)
		r.cat.close()
		r.cat = nil
	}
	return nil, false
}

// Files lists every tracked file at a commit.
func (r *Repo) Files(sha string) ([]string, error) {
	if unsafe(sha) {
		return nil, errUnsafe
	}
	out, err := r.run("ls-tree", "-r", "-z", "--name-only", sha) // -z: names end with NUL, so spaces stay in them
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// Churn counts, for each file, how many of the last limit commits changed it.
func (r *Repo) Churn(limit int) map[string]int {
	out, err := r.run("log", "-n", strconv.Itoa(limit), "--format=", "--name-only")
	counts := map[string]int{}
	if err != nil {
		return counts
	}
	for _, p := range strings.Split(out, "\n") {
		if p = strings.TrimSpace(p); p != "" {
			counts[p]++
		}
	}
	return counts
}

// LastChange is the commit that last changed one line of a file at HEAD.
func (r *Repo) LastChange(path string, line int) (Commit, bool) {
	out, err := r.run("blame", "--porcelain", "-L", fmt.Sprintf("%d,%d", line, line), "HEAD", "--", path)
	if err != nil || len(out) < 40 {
		return Commit{}, false
	}
	c, err := r.Get(out[:40])
	return c, err == nil
}

// Count is how many commits HEAD has.
func (r *Repo) Count() int {
	out, err := r.run("rev-list", "--count", "HEAD")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// Resolve turns a branch name or commit into a full commit id, or "" if there is none.
func (r *Repo) Resolve(ref string) string {
	if unsafe(ref) {
		return ""
	}
	out, err := r.run("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ReadAll reads many files at one commit through a single git process, which is far faster than
// one process per file. Files that do not exist are left out.
func (r *Repo) ReadAll(ref string, paths []string) map[string][]byte {
	out := map[string][]byte{}
	if len(paths) == 0 || unsafe(ref) {
		return out
	}
	cmd := exec.Command("git", "-C", r.Root, "cat-file", "--batch")
	var in bytes.Buffer
	for _, p := range paths {
		in.WriteString(ref + ":" + p + "\n")
	}
	cmd.Stdin = &in
	raw, err := cmd.Output()
	if err != nil {
		return out
	}
	for _, p := range paths {
		nl := bytes.IndexByte(raw, '\n')
		if nl < 0 {
			break
		}
		header := strings.Fields(string(raw[:nl]))
		raw = raw[nl+1:]
		if len(header) < 3 || header[1] != "blob" {
			continue // "missing" has no body
		}
		size, _ := strconv.Atoi(header[2])
		if size > len(raw) {
			break
		}
		out[p] = raw[:size]
		raw = raw[min(size+1, len(raw)):]
	}
	return out
}

// Ref is a branch, local or remote.
type Ref struct {
	Name    string    `json:"name"`
	Remote  bool      `json:"remote"`
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	By      string    `json:"by"`
	At      time.Time `json:"-"`
}

// Refs lists local and remote branches, most recently changed first.
func (r *Repo) Refs() []Ref {
	out, err := r.run("for-each-ref", "--sort=-committerdate", "--format=%(refname)"+sep+"%(objectname)"+sep+"%(subject)"+sep+"%(authorname)"+sep+"%(committerdate:unix)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil
	}
	var refs []Ref
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, sep)
		if len(f) < 5 || strings.HasSuffix(f[0], "/HEAD") {
			continue
		}
		at, _ := strconv.ParseInt(f[4], 10, 64)
		name, remote := strings.TrimPrefix(f[0], "refs/heads/"), strings.HasPrefix(f[0], "refs/remotes/")
		if remote {
			name = strings.TrimPrefix(f[0], "refs/remotes/")
		}
		refs = append(refs, Ref{Name: name, Remote: remote, SHA: f[1], Message: f[2], By: f[3], At: time.Unix(at, 0)})
	}
	return refs
}

// Authors counts commits per author on ref.
func (r *Repo) Authors(ref string) map[string]int {
	counts := map[string]int{}
	if unsafe(ref) {
		return counts
	}
	out, err := r.run("shortlog", "-sn", "--no-merges", ref, "--")
	if err != nil {
		return counts
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		n, name, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if c, err := strconv.Atoi(strings.TrimSpace(n)); ok && err == nil {
			counts[strings.TrimSpace(name)] = c
		}
	}
	return counts
}

// CountRef is how many commits ref has.
func (r *Repo) CountRef(ref string) int {
	if unsafe(ref) {
		return 0
	}
	out, err := r.run("rev-list", "--count", ref, "--")
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

// IsAncestor says commit a comes before commit b in b's history (a commit is not its own ancestor here).
func (r *Repo) IsAncestor(a, b string) bool {
	if a == "" || b == "" || a == b || unsafe(a, b) {
		return false
	}
	_, err := r.run("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Later lists the commits after sha up to HEAD that changed path, oldest first: the ones that build on sha, so a
// side branch merged later is left out. It is empty when sha is HEAD or not an ancestor of it.
func (r *Repo) Later(sha, path string) []string {
	if unsafe(sha) {
		return nil
	}
	out, err := r.run("rev-list", "--reverse", "--ancestry-path", sha+"..HEAD", "--", path)
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

// LogFile lists up to limit commits that changed one file, newest first.
func (r *Repo) LogFile(path string, limit int) []Commit {
	out, err := r.run("log", "-n", strconv.Itoa(limit), "--format=%H"+sep+"%an"+sep+"%at"+sep+"%P"+sep+"%s"+sep+"%b"+end, "HEAD", "--", path)
	if err != nil {
		return nil
	}
	var cs []Commit
	for _, rec := range strings.Split(out, end) {
		f := strings.Split(strings.TrimLeft(rec, "\n"), sep)
		if len(f) < 6 {
			continue
		}
		at, _ := strconv.ParseInt(f[2], 10, 64)
		cs = append(cs, Commit{SHA: f[0], Short: f[0][:min(7, len(f[0]))], Author: f[1], Time: time.Unix(at, 0), Parents: strings.Fields(f[3]), Subject: f[4], Body: strings.TrimSpace(f[5])})
	}
	return cs
}
