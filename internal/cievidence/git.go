package cievidence

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Git runs git in Dir.
type Git struct{ Dir string }

func (g Git) run(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = g.Dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Head is the checked-out commit.
func (g Git) Head() (string, error) { return g.run("rev-parse", "HEAD") }

// Ref is the branch, or "" when detached.
func (g Git) Ref() string {
	r, _ := g.run("symbolic-ref", "-q", "HEAD")
	return r
}

// Remote is origin's URL as a repository name (see RepoName), or "".
func (g Git) Remote() string {
	u, _ := g.run("config", "--get", "remote.origin.url")
	return RepoName(u)
}

var scpLike = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):(.+)$`)

// RepoName is a remote URL as "git+https://host/path", with any user name,
// password or token removed and no ".git" suffix. A CI checkout's remote
// often embeds a token (https://x-access-token:...@github.com/...): it must
// never reach a statement.
func RepoName(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	host, path := "", ""
	if u, err := url.Parse(remote); err == nil && u.Host != "" {
		host, path = u.Hostname(), u.Path
		if p := u.Port(); p != "" && u.Scheme != "ssh" {
			host += ":" + p
		}
	} else if m := scpLike.FindStringSubmatch(remote); m != nil {
		host, path = m[1], "/"+m[2]
	} else {
		return ""
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	return "git+https://" + host + path
}

// Commit is one commit's author line and message, for agent detection only.
type commitText struct{ author, message string }

func (g Git) commits(base, head string) ([]commitText, error) {
	rng := head
	args := []string{"log", "--format=%an <%ae>%x00%B%x01"}
	if base != "" {
		rng = base + ".." + head
	} else {
		args = append(args, "-1")
	}
	out, err := g.run(append(args, rng)...)
	if err != nil {
		return nil, err
	}
	var cs []commitText
	for _, rec := range strings.Split(out, "\x01") {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		author, msg, _ := strings.Cut(rec, "\x00")
		cs = append(cs, commitText{author, msg})
	}
	return cs, nil
}

// agentMarks name coding agents by what they write into commits: an author,
// a co-author trailer or a generated-with line. Matched case-insensitively.
var agentMarks = []struct {
	agent string
	re    *regexp.Regexp
}{
	{"claude", regexp.MustCompile(`(?i)co-authored-by:\s*claude|noreply@anthropic\.com|generated with \[?claude code`)},
	{"copilot", regexp.MustCompile(`(?i)co-authored-by:\s*copilot|\+copilot@users\.noreply\.github\.com|^copilot <`)},
	{"cursor", regexp.MustCompile(`(?i)cursoragent@cursor\.com|co-authored-by:\s*cursor`)},
	{"aider", regexp.MustCompile(`(?i)^aider:|\(aider\) <|co-authored-by:\s*aider`)},
	{"devin", regexp.MustCompile(`(?i)devin-ai-integration|co-authored-by:\s*devin`)},
	{"codex", regexp.MustCompile(`(?i)co-authored-by:\s*(openai\s+)?codex|chatgpt-codex-connector`)},
	{"gemini", regexp.MustCompile(`(?i)co-authored-by:\s*gemini|gemini-code-assist`)},
	{"jules", regexp.MustCompile(`(?i)google-labs-jules|co-authored-by:\s*jules`)},
	{"amazon-q", regexp.MustCompile(`(?i)amazon-q-developer|co-authored-by:\s*amazon q`)},
}

// Agents are the coding agents a commit names.
func Agents(author, message string) []string {
	text := author + "\n" + message
	var out []string
	for _, m := range agentMarks {
		for _, line := range strings.Split(text, "\n") {
			if m.re.MatchString(strings.TrimSpace(line)) {
				out = append(out, m.agent)
				break
			}
		}
	}
	return out
}

// AgentCommitsIn counts the commits in base..head (head alone when base is
// empty) that name a coding agent.
func (g Git) AgentCommitsIn(base, head string) (*AgentCommits, error) {
	cs, err := g.commits(base, head)
	if err != nil {
		return nil, err
	}
	a := &AgentCommits{Range: head, Commits: len(cs), ByAgent: map[string]int{}}
	if base != "" {
		a.Range = base + ".." + head
	}
	for _, c := range cs {
		names := Agents(c.author, c.message)
		if len(names) > 0 {
			a.WithAgent++
		}
		for _, n := range names {
			a.ByAgent[n]++
		}
	}
	return a, nil
}

// PatchedWorktree checks HEAD out into a temporary worktree and applies
// patch there, for a canary run on deliberately broken code; the checkout
// the command was started in is never touched. It returns the directory to
// run in (the same subdirectory the caller is in) and a cleanup.
func (g Git) PatchedWorktree(patch string) (string, func(), error) {
	abs, err := filepath.Abs(patch)
	if err != nil {
		return "", nil, err
	}
	prefix, err := g.run("rev-parse", "--show-prefix")
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "tellproof-canary-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_, _ = g.run("worktree", "remove", "--force", dir)
		_ = os.RemoveAll(dir)
		_, _ = g.run("worktree", "prune")
	}
	if _, err := g.run("worktree", "add", "--detach", "--quiet", dir, "HEAD"); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	if _, err := (Git{Dir: dir}).run("apply", "--whitespace=nowarn", abs); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("cievidence: the canary patch does not apply to HEAD: %w", err)
	}
	return filepath.Join(dir, prefix), cleanup, nil
}
