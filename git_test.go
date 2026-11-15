package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/abxengg/codist-git/testrepo"
)

// newRepo makes a repository with two commits: one adds A.java, the next changes line 3 and adds line 5.
func newRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	write := func(s string) { os.WriteFile(filepath.Join(dir, "A.java"), []byte(s), 0o644) }
	run("init", "-q")
	write("class A {\n  int a() {\n    return 1;\n  }\n}\n")
	run("add", ".")
	run("commit", "-q", "-m", "Add A")
	write("class A {\n  int a() {\n    return 2;\n  }\n  int b() { return 3; }\n}\n")
	run("commit", "-q", "-am", "Change A", "-m", "With a body.")
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLogAndChanges(t *testing.T) {
	r := newRepo(t)
	log, err := r.Log(10)
	if err != nil || len(log) != 2 {
		t.Fatalf("log: %v %v", log, err)
	}
	if log[0].Subject != "Change A" || log[0].Body != "With a body." || len(log[0].Parents) != 1 || log[1].Subject != "Add A" {
		t.Errorf("commits: %+v", log)
	}
	ch, err := r.Changes(log[0].SHA)
	if err != nil || len(ch) != 1 {
		t.Fatalf("changes: %v %v", ch, err)
	}
	if ch[0].Path != "A.java" || ch[0].Status != "modified" || !slices.Equal(ch[0].Added, []int{3, 5}) {
		t.Errorf("change: %+v", ch[0])
	}
	first, _ := r.Changes(log[1].SHA)
	if len(first) != 1 || first[0].Status != "added" || len(first[0].Added) != 5 {
		t.Errorf("first commit: %+v", first)
	}
	if src, ok := r.FileAt(log[1].SHA, "A.java"); !ok || string(src) == "" {
		t.Error("FileAt failed")
	}
	if c, ok := r.LastChange("A.java", 5); !ok || c.SHA != log[0].SHA {
		t.Errorf("blame: %+v", c)
	}
	if all := r.ReadAll(log[1].SHA, []string{"A.java", "missing.java"}); len(all) != 1 || !strings.Contains(string(all["A.java"]), "return 1;") {
		t.Errorf("ReadAll: %q", all)
	}
	if refs := r.Refs(); len(refs) != 1 || refs[0].SHA != log[0].SHA || r.Resolve(refs[0].Name) != log[0].SHA {
		t.Errorf("refs: %+v", refs)
	}
	if r.Authors("HEAD")["T"] != 2 || r.CountRef("HEAD") != 2 {
		t.Error("authors or count wrong")
	}
	if r.Count() != 2 || r.Churn(10)["A.java"] != 2 {
		t.Error("count or churn wrong")
	}
}

func TestLater(t *testing.T) {
	r := newRepo(t)
	log, _ := r.Log(10)
	if got := r.Later(log[1].SHA, "A.java"); !slices.Equal(got, []string{log[0].SHA}) {
		t.Errorf("later than the first commit: %v", got)
	}
	if got := r.Later(log[0].SHA, "A.java"); len(got) != 0 {
		t.Errorf("nothing is later than HEAD: %v", got)
	}
	if got := r.Later(log[1].SHA, "B.java"); len(got) != 0 {
		t.Errorf("no commit changed B.java: %v", got)
	}
}

func TestIsAncestor(t *testing.T) {
	r := newRepo(t)
	log, _ := r.Log(10)
	if !r.IsAncestor(log[1].SHA, log[0].SHA) || r.IsAncestor(log[0].SHA, log[1].SHA) || r.IsAncestor(log[0].SHA, log[0].SHA) {
		t.Error("ancestry is wrong")
	}
	if r.IsAncestor("baseline", log[0].SHA) {
		t.Error("a name that is not a commit is no ancestor")
	}
}

// Show reads the commit and its changes together, and for a merge only what was written by hand to settle it.
func TestShowAndMerges(t *testing.T) {
	tr := testrepo.New(t)
	r, err := Open(tr.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c, files, err := r.Show(tr.Bug)
	if err != nil || c.SHA != tr.Bug || c.Subject != "Add discount" || len(files) != 1 || files[0].Path != testrepo.ServicePath || len(files[0].Added) == 0 {
		t.Fatalf("%+v %+v %v", c, files, err)
	}
	// Both sides change the same line; the person merging writes a third version.
	const p = "order-service/src/main/resources/application.yml"
	tr.Git("checkout", "-q", "-b", "side")
	tr.Commit("Port 9001", map[string]string{p: "spring:\n  application:\n    name: order-service\nserver:\n  port: 9001\n"})
	tr.Git("checkout", "-q", "main")
	tr.Commit("Port 9002", map[string]string{p: "spring:\n  application:\n    name: order-service\nserver:\n  port: 9002\n"})
	if out, err := exec.Command("git", "-C", tr.Dir, "merge", "side").CombinedOutput(); err == nil {
		t.Fatalf("expected a conflict: %s", out)
	}
	tr.Commit("Merge side", map[string]string{p: "spring:\n  application:\n    name: order-service\nserver:\n  port: 9003\n"})
	c, files, err = r.Show(tr.Head())
	if err != nil || len(c.Parents) != 2 {
		t.Fatalf("%+v %v", c, err)
	}
	if len(files) != 1 || files[0].Path != p || !strings.Contains(files[0].Patch, "+  port: 9003") {
		t.Errorf("a merge shows what was written to settle it: %+v", files)
	}
	// A merge that needed no hand changes nothing.
	tr.Git("checkout", "-q", "-b", "clean", "HEAD~1")
	tr.Commit("Other file", map[string]string{"NOTES.md": "notes\n"})
	tr.Git("checkout", "-q", "main")
	tr.Git("merge", "-q", "--no-ff", "-m", "Merge clean", "clean")
	if _, files, err := r.Show(tr.Head()); err != nil || len(files) != 0 {
		t.Errorf("a clean merge: %+v %v", files, err)
	}
}

// Head is read from the repository's files when it can be, and always agrees with git.
func TestHeadFromFiles(t *testing.T) {
	tr := testrepo.New(t)
	r, err := Open(tr.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	same := func(what string) {
		t.Helper()
		if want := tr.Git("rev-parse", "HEAD"); r.Head() != want {
			t.Errorf("%s: Head %q, git says %q", what, r.Head(), want)
		}
	}
	if r.headFromFiles() != tr.Fix {
		t.Errorf("on a branch, read from files: %q", r.headFromFiles())
	}
	same("on a branch")
	tr.Commit("More", map[string]string{"NOTES.md": "x\n"})
	same("after a commit")
	tr.Git("checkout", "-q", "--detach", tr.Bug)
	if r.headFromFiles() != tr.Bug {
		t.Error("a detached HEAD is read from files")
	}
	same("detached")
	tr.Git("checkout", "-q", "main")
	tr.Git("pack-refs", "--all")
	if r.headFromFiles() != "" {
		t.Error("a packed ref is left to git")
	}
	same("packed refs")
}
