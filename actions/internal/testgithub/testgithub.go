// Package testgithub is what the tests need of this repository's GitHub
// emulator beyond the emulator itself: a job's environment, as GitHub gives
// a step its variables, event, files and GITHUB_TOKEN, and the tokens
// handed out, which tests check no log shows. It's for tests only.
package testgithub

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ghemu "github.com/lanescape/runners/emulators/github"
)

// JobsAppID is the ID of the App whose installation tokens stand in for
// jobs' GITHUB_TOKENs, as GitHub's own Actions App's do.
const JobsAppID = 15368

// GitHub is a GitHub emulator and the jobs' token issuer.
type GitHub struct {
	*ghemu.Emulator
	t    *testing.T
	jobs *ghemu.App

	mu        sync.Mutex
	installed map[int64]*ghemu.Installation

	// Minted are the installation tokens handed to jobs, which tests check
	// no log shows.
	Minted []string
}

// New starts an emulator, closed when the test ends.
func New(t *testing.T) *GitHub {
	t.Helper()
	e := ghemu.New()
	t.Cleanup(e.Close)
	return &GitHub{Emulator: e, t: t, jobs: e.NewApp(JobsAppID), installed: map[int64]*ghemu.Installation{}}
}

// JobToken is a GITHUB_TOKEN for a job of a run in a repository of owner:
// an installation token, which reads that owner's repositories and public
// ones.
func (g *GitHub) JobToken(owner *ghemu.Account) string {
	g.t.Helper()
	g.mu.Lock()
	in := g.installed[owner.ID]
	if in == nil {
		in = g.Install(g.jobs, owner)
		g.installed[owner.ID] = in
	}
	g.mu.Unlock()
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", g.URL, in.ID), nil)
	req.Header.Set("Authorization", "Bearer "+appJWT(g.jobs))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Token == "" {
		g.t.Fatalf("no installation token: %d %v", resp.StatusCode, err)
	}
	g.mu.Lock()
	g.Minted = append(g.Minted, out.Token)
	g.mu.Unlock()
	return out.Token
}

// appJWT is an App's JWT, RS256, as GitHub asks.
func appJWT(a *ghemu.App) string {
	enc := base64.RawURLEncoding
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "iss": strconv.FormatInt(a.ID, 10)})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signing + "." + enc.EncodeToString(sig)
}

// Secrets are every token handed to a job so far.
func (g *GitHub) Secrets() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.Minted...)
}

// Job is a job of a run, as a step sees it.
type Job struct {
	Run *ghemu.Run
	// Pull is the event's pull request, for pull_request and
	// pull_request_target runs.
	Pull *ghemu.Pull
	// Inputs are the action's inputs, as INPUT_<NAME>.
	Inputs map[string]string
}

// Env is the job's environment, as GitHub gives a step: its GITHUB_
// variables, its event's payload, output and summary files, and its
// GITHUB_TOKEN.
func (g *GitHub) Env(j Job) map[string]string {
	g.t.Helper()
	dir := g.t.TempDir()
	run := j.Run
	payload := map[string]any{
		"repository": map[string]any{"id": run.Repo.ID, "full_name": run.Repo.FullName(), "owner": map[string]any{"id": run.Repo.Owner.ID, "login": run.Repo.Owner.Login}},
	}
	if p := j.Pull; p != nil {
		// The event is what it was when the run was made, as GitHub keeps
		// it, though the pull request has moved on since.
		number, sha := p.Number, p.HeadSHA
		if ep := run.EventPull; ep != nil {
			number, sha = ep.Number, ep.HeadSHA
		}
		var head any
		if p.Head != nil {
			head = map[string]any{"id": p.Head.ID, "full_name": p.Head.FullName()}
		}
		payload["number"] = number
		payload["pull_request"] = map[string]any{"number": number, "head": map[string]any{"sha": sha, "ref": p.HeadRef, "repo": head}}
	}
	b, _ := json.Marshal(payload)
	eventPath := filepath.Join(dir, "event.json")
	if err := os.WriteFile(eventPath, b, 0o644); err != nil {
		g.t.Fatal(err)
	}
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		g.t.Fatal(err)
	}
	env := map[string]string{
		"GITHUB_ACTIONS":             "true",
		"GITHUB_EVENT_NAME":          run.Event,
		"GITHUB_EVENT_PATH":          eventPath,
		"GITHUB_REPOSITORY":          run.Repo.FullName(),
		"GITHUB_REPOSITORY_ID":       strconv.FormatInt(run.Repo.ID, 10),
		"GITHUB_REPOSITORY_OWNER":    run.Repo.Owner.Login,
		"GITHUB_REPOSITORY_OWNER_ID": strconv.FormatInt(run.Repo.Owner.ID, 10),
		"GITHUB_SHA":                 run.SHA,
		"GITHUB_REF":                 run.Ref,
		"GITHUB_RUN_ID":              strconv.FormatInt(run.ID, 10),
		"GITHUB_API_URL":             g.URL,
		"GITHUB_OUTPUT":              filepath.Join(dir, "output"),
		"GITHUB_STEP_SUMMARY":        filepath.Join(dir, "summary"),
		"GITHUB_WORKSPACE":           ws,
		"RUNNER_TEMP":                dir,
		// A job may give its steps its token, as many do; no action
		// reads it, and no log may show it.
		"GITHUB_TOKEN": g.JobToken(run.Repo.Owner),
	}
	for k, v := range j.Inputs {
		env["INPUT_"+strings.ToUpper(strings.ReplaceAll(k, "-", "_"))] = v
	}
	return env
}

// Outputs reads a step's outputs from its output file.
func Outputs(t *testing.T, env map[string]string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(env["GITHUB_OUTPUT"])
	if os.IsNotExist(err) {
		return map[string]string{}
	}
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		name, delim, ok := strings.Cut(lines[i], "<<")
		if !ok {
			continue
		}
		var v []string
		for i++; i < len(lines) && lines[i] != delim; i++ {
			v = append(v, lines[i])
		}
		out[name] = strings.Join(v, "\n")
	}
	return out
}

// Summary reads a step's summary.
func Summary(t *testing.T, env map[string]string) string {
	t.Helper()
	b, _ := os.ReadFile(env["GITHUB_STEP_SUMMARY"])
	return string(b)
}
