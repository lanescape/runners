// Command checkout is the checkout action's program (specs/0007-actions.md,
// checkout). The action checks out with actions/checkout; this program
// decides what it checks out, before, and confirms what it checked out,
// after:
//
//	checkout decide   the repository, commit and credentials to check out
//	checkout verify   that a contribution's checkout is exactly its commit,
//	                  with no credentials left behind
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/lanescape/runners/actions/internal/actions"
	"github.com/lanescape/runners/actions/internal/event"
	"github.com/lanescape/runners/actions/internal/where"
)

func main() {
	rt := actions.Default()
	var err error
	switch {
	case len(os.Args) == 2 && os.Args[1] == "decide":
		err = decide(rt)
	case len(os.Args) == 2 && os.Args[1] == "verify":
		err = verify(context.Background(), rt)
	default:
		err = errors.New("usage: checkout decide|verify")
	}
	if err != nil {
		actions.Fail(err)
	}
}

// decide sets the outputs the action passes to actions/checkout:
// repository and ref, empty for actions/checkout's own defaults, the run's
// own commit; persist-credentials; allow-unsafe-pr-checkout, which
// actions/checkout requires to check out a fork's commit in a
// pull_request_target run; and expect, the commit verify confirms.
func decide(rt *actions.Runtime) error {
	contribution, err := rt.BoolInput("contribution", false)
	if err != nil {
		return err
	}
	r, err := event.Load(rt.Getenv)
	if err != nil {
		return err
	}
	place := where.Of(r)
	out := map[string]string{"repository": "", "ref": "", "persist-credentials": "true", "allow-unsafe-pr-checkout": "false", "expect": ""}
	what := "the run's own commit, as actions/checkout does"
	if contribution {
		n, head, sha, err := r.Contribution()
		if err != nil {
			return fmt.Errorf("contribution: true checks out an approved contribution only in a pull_request_target run, and refuses here: %w", err)
		}
		out = map[string]string{"repository": head.FullName, "ref": sha, "persist-credentials": "false", "allow-unsafe-pr-checkout": "true", "expect": sha}
		what = fmt.Sprintf("pull request %d's head commit, %s, from %s, with no credentials persisted", n, sha, head.FullName)
	}
	for _, name := range []string{"repository", "ref", "persist-credentials", "allow-unsafe-pr-checkout", "expect"} {
		if err := rt.SetOutput(name, out[name]); err != nil {
			return err
		}
	}
	rt.Logf("Checking out %s.", what)
	return rt.Summary(fmt.Sprintf("## Checkout\n\n%s\n\nChecked out %s.\n\n", place.Summary(), what))
}

// verify confirms that the checkout in the workspace is the commit expected
// and that no credentials were persisted beside it. With nothing expected,
// as for the run's own commit, it does nothing.
func verify(ctx context.Context, rt *actions.Runtime) error {
	expect := rt.Input("expect")
	if expect == "" {
		return nil
	}
	dir := rt.Env("GITHUB_WORKSPACE")
	if dir == "" {
		return errors.New("GITHUB_WORKSPACE isn't set")
	}
	head, err := git(ctx, dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("reading the checkout's commit: %w", err)
	}
	if head != expect {
		// What Git printed is shown only if it's a commit's name.
		if !commitName.MatchString(head) {
			return fmt.Errorf("the checkout isn't the contribution's commit, %s", expect)
		}
		return fmt.Errorf("the checkout is %s, not the contribution's commit, %s", head, expect)
	}
	// actions/checkout persists credentials as an http extraheader, in
	// the repository's config or in a file it includes.
	cfg, err := git(ctx, dir, "config", "--local", "--name-only", "--list")
	if err != nil {
		return fmt.Errorf("reading the checkout's git config: %w", err)
	}
	for name := range strings.SplitSeq(cfg, "\n") {
		if kind := credentialKind(name); kind != "" {
			return fmt.Errorf("the checkout's git config keeps %s, which may hold credentials", kind)
		}
	}
	rt.Logf("The checkout is %s, with no credentials persisted.", head)
	return nil
}

// commitName is a commit's full name, SHA-1 or SHA-256.
var commitName = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// credentialKind is the kind of git config entry name is, if it's one that
// may hold credentials, and otherwise "". Only the kind is ever shown, never
// the name: a name can hold a credential itself, as in
// credential.https://user:password@example.com.helper.
func credentialKind(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".extraheader"):
		return "an extraheader entry"
	case strings.HasPrefix(n, "includeif."):
		return "an includeIf entry"
	case strings.HasPrefix(n, "credential."):
		return "a credential entry"
	}
	return ""
}

// git runs git in dir and gives what it printed. A failure is told by git's
// exit status alone, never by what git printed on stderr, which can repeat
// the repository's config, credentials included.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	// The workspace may belong to another user, such as root, if an earlier
	// step ran as root with sudo.
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=" + dir, "-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("git %s exited with status %d", args[0], ee.ExitCode())
		}
		return "", fmt.Errorf("git %s couldn't run", args[0])
	}
	return strings.TrimSpace(string(out)), nil
}
