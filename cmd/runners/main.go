// Command runners is the service, on Cloud Run (specs/0001-architecture.md).
//
// Its configuration is embedded (config/), and what differs by deployment
// comes from the environment, set by Terraform:
//
//	PORT                         where to listen; Cloud Run sets it
//	RUNNERS_AUDIENCE             the audience callers' tokens must name: the service's URL
//	RUNNERS_PROJECT              the project the service's machines are in
//	RUNNERS_BUCKET               the service's state bucket
//	RUNNERS_GITHUB_APP_ID        the GitHub App's ID
//	RUNNERS_GITHUB_APP_KEY_FILE  the App's private key, mounted from Secret Manager
//	RUNNERS_WEBHOOK_SECRET_FILE  the App's webhook secret, mounted from Secret Manager
//	RUNNERS_RECONCILE_ACCOUNTS   the scheduler's and push subscriptions' accounts, comma-separated
//
// The issuers it trusts are Google's and GitHub's own, and aren't
// configurable. It refuses to start unless the organization requires
// approval for every outside contributor's fork workflows.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/config"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/compute"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/machines"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
)

const githubAPI = "https://api.github.com"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Getenv, log); err != nil {
		log.Error("runners stopped", "err", err)
		os.Exit(1)
	}
}

// settings are what comes from the environment.
type settings struct {
	Port              string
	Audience          string
	Project           string
	Bucket            string
	AppID             int64
	AppKeyFile        string
	WebhookSecretFile string
	ReconcileAccounts []string
}

func loadSettings(getenv func(string) string) (settings, error) {
	s := settings{
		Port:              getenv("PORT"),
		Audience:          getenv("RUNNERS_AUDIENCE"),
		Project:           getenv("RUNNERS_PROJECT"),
		Bucket:            getenv("RUNNERS_BUCKET"),
		AppKeyFile:        getenv("RUNNERS_GITHUB_APP_KEY_FILE"),
		WebhookSecretFile: getenv("RUNNERS_WEBHOOK_SECRET_FILE"),
	}
	if s.Port == "" {
		s.Port = "8080"
	}
	var errs []error
	for name, v := range map[string]string{
		"RUNNERS_AUDIENCE":            s.Audience,
		"RUNNERS_PROJECT":             s.Project,
		"RUNNERS_BUCKET":              s.Bucket,
		"RUNNERS_GITHUB_APP_KEY_FILE": s.AppKeyFile,
		"RUNNERS_WEBHOOK_SECRET_FILE": s.WebhookSecretFile,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	if !strings.HasPrefix(s.Audience, "https://") && s.Audience != "" {
		errs = append(errs, errors.New("RUNNERS_AUDIENCE must be the service's https URL"))
	}
	id, err := strconv.ParseInt(getenv("RUNNERS_GITHUB_APP_ID"), 10, 64)
	if err != nil || id <= 0 {
		errs = append(errs, errors.New("RUNNERS_GITHUB_APP_ID must be the App's numeric ID"))
	}
	s.AppID = id
	for _, a := range strings.Split(getenv("RUNNERS_RECONCILE_ACCOUNTS"), ",") {
		if a = strings.TrimSpace(a); a != "" {
			if !strings.HasSuffix(a, ".gserviceaccount.com") {
				errs = append(errs, fmt.Errorf("RUNNERS_RECONCILE_ACCOUNTS: %q isn't a service account", a))
			}
			s.ReconcileAccounts = append(s.ReconcileAccounts, a)
		}
	}
	if len(s.ReconcileAccounts) == 0 {
		errs = append(errs, errors.New("RUNNERS_RECONCILE_ACCOUNTS is required"))
	}
	return s, errors.Join(errs...)
}

func readSecret(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return b, nil
}

func run(ctx context.Context, getenv func(string) string, log *slog.Logger) error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	s, err := loadSettings(getenv)
	if err != nil {
		return err
	}
	key, err := readSecret(s.AppKeyFile)
	if err != nil {
		return fmt.Errorf("github app key: %w", err)
	}
	webhookSecret, err := readSecret(s.WebhookSecretFile)
	if err != nil {
		return fmt.Errorf("webhook secret: %w", err)
	}
	gh, err := github.NewAppClient(github.Options{BaseURL: githubAPI, AppID: s.AppID, PrivateKey: key, OrganizationID: cfg.Organization.ID})
	if err != nil {
		return err
	}
	objects, err := store.NewGCS(ctx, s.Bucket)
	if err != nil {
		return err
	}
	defer objects.Close()
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.GoogleIssuer, Audience: s.Audience}, s.ReconcileAccounts)
	if err != nil {
		return err
	}
	actions, err := auth.NewActions(auth.Options{Issuer: auth.ActionsIssuer, Audience: s.Audience}, cfg.Organization.ID)
	if err != nil {
		return err
	}
	account := machines.MachineAccountOf(s.Project)
	machineTokens, err := auth.NewMachines(auth.Options{Issuer: auth.GoogleIssuer, Audience: s.Audience}, s.Project, account)
	if err != nil {
		return err
	}
	gce, err := compute.New(ctx, s.Project)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = machines.CheckForkApproval(cctx, gh)
	cancel()
	if err != nil {
		return fmt.Errorf("refusing to run: %w", err)
	}
	clock := clockwork.NewRealClock()
	binder := &binding.Binder{GitHub: gh, Objects: objects, OrganizationID: cfg.Organization.ID, Clock: clock, Logger: log}
	svc := &machines.Service{Config: cfg, Project: s.Project, ServiceURL: s.Audience, MachineAccount: account,
		GitHub: gh, Compute: gce, Objects: objects, Binder: binder, Machines: machineTokens, Clock: clock, Logger: log}
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}
	svc.Kick = kicker(ctx, passes, svc.Pass, 15*time.Second, log)
	h, err := server.New(server.Config{
		Logger:           log,
		OrganizationID:   cfg.Organization.ID,
		WebhookSecret:    webhookSecret,
		Publisher:        noPublisher{},
		ReconcileCallers: callers,
		Passes:           passes,
		Reconciler:       svc,
		Actions:          actions,
		Binder:           binder,
		JobStarted:       svc.JobStarted(),
		JobFinished:      svc.JobFinished(),
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              ":" + s.Port,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// Above the pass's deadline, as Cloud Run's request timeout is.
		WriteTimeout: 10 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("runners serving", "port", s.Port)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// kicker runs a pass soon after a machine reports, so its cleanup starts at
// once. Reports that arrive before a pass is tried share it. A report is
// pending until a pass that began after it has run: one that finds the
// lease held tries again, since the pass holding it may have read the
// records before the report arrived, until the lease has had time to
// expire. The sweep catches whatever this misses.
func kicker(ctx context.Context, passes *pass.Runner, body pass.Body, every time.Duration, log *slog.Logger) func() {
	pending := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pending:
			}
			for end := time.Now().Add(pass.Validity + pass.Deadline); ; {
				err := passes.Run(ctx, body)
				if !errors.Is(err, pass.ErrHeld) {
					if err != nil {
						log.Warn("pass after a report", "err", err)
					}
					break
				}
				if time.Now().After(end) {
					log.Warn("no pass after a report; the sweep will catch it")
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(every):
				}
			}
		}
	}()
	return func() {
		select {
		case pending <- struct{}{}:
		default:
		}
	}
}

// noPublisher stands in until the service publishes to Pub/Sub: a delivery
// gets a retryable error, and passes' sweeps catch what it would have
// started.
type noPublisher struct{}

func (noPublisher) Publish(context.Context, server.Delivery) error {
	return errors.New("publishing to Pub/Sub isn't built yet")
}
