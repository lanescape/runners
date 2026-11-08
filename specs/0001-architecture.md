# Architecture

Lanescape Runners is a GitHub Actions runner service for the repositories of the `lanescape` GitHub organization, and for no one else, on Google Cloud. A job asks for a machine by label, as it would ask Blacksmith or GitHub: `runs-on: lanescape-80vcpu-linux-amd64`. The service creates a Compute Engine machine for that job, registers a one-job runner on it, and deletes it afterwards. A job's work disk can be saved when it finishes, and later jobs start from it (specs/0003-disks.md). Jobs read upstream sources through a caching proxy (specs/0006-proxy.md). Nothing runs while no job is queued, apart from the proxy's git mirror and storage. Repositories that use it are **consumers**: any repository of the organization.

The service provides compute, saved disks and the proxy. It never decides what a job may do in a consumer's own cloud resources, and a job's machine has no identity that can reach them: its account has no role anywhere (specs/0002-machines.md). A job that needs a consumer's resources signs in with GitHub's OIDC token through that consumer's own workload identity federation.

## Only the `lanescape` organization

The code is public under the MIT License, so anyone may read or copy it. Only the `lanescape` organization may use the service, and it's built so that no one else can:

1. **The GitHub App is private to the organization.** It's set to install only on its owner's account, so no other organization or person can install it, and the service registers runners nowhere else.
2. **Every check is by ID.** The organization's ID is in the service's configuration; a repository is a consumer if its owner ID is that, whatever its name. Every endpoint checks the owner ID GitHub reports, so a renamed or look-alike organization never matches.
3. **Runners register only in the organization's runner groups,** one per repository, made by the service (below).
4. **Machines, saved disks and images are private to the project.** Compute Engine images are visible only within their project unless shared, and the service never shares one. The service's buckets enforce public access prevention, and the proxy serves only callers it has authenticated (specs/0006-proxy.md). Nothing the service stores can be read from outside the organization, so no one else's reads cost it anything.
5. **Forks get nothing of the service's** (below), and the actions in `lanescape/actions` do nothing of the service's outside the organization. Anyone can reference a public action; it falls back to GitHub's runners and plain reads from upstream, and the service and the proxy refuse it anyway.

A consumer's own buckets and identities are the consumer's to keep private: public access prevention on, and no sign-in for a fork's job or for a `pull_request_target` run.

## Components

All of it runs in project `lanescape-dev`, region `northamerica-northeast1`, and is defined in this repository's Terraform (specs/0004-deployment.md).

- **The service,** one Go program on Cloud Run, with five endpoints: GitHub's webhook, the reconcile endpoint, the run binding, the job-started check and the job-finished report.
- **The proxy** (specs/0006-proxy.md): a Go program on Cloud Run for HTTP reads, and one small Compute Engine machine for git mirrors.
- **A GitHub App,** "Lanescape Runners", installed on the organization. It creates and manages a runner group per consumer, registers just-in-time runners in them, lists jobs and reruns runs, and reads consumers' default-branch files. Its webhook delivers `workflow_job` and `workflow_run` events.
- **Pub/Sub.** The webhook endpoint checks the delivery's signature and publishes it to a topic, and a push subscription delivers it to the reconcile endpoint. A log sink publishes Compute Engine's preemption and guest-shutdown events for the service's machines to another topic, pushed to the same endpoint, which records each preemption in the bucket before acknowledging it.
- **Cloud Scheduler** calls the reconcile endpoint every 5 minutes. Events can be lost, delayed or duplicated, so a pass works from what GitHub and Compute Engine report when it runs, never from the event that started it, and the sweep catches anything an event missed.
- **Buckets:** `lanescape-runners` for the service's state (the lease, records of preemptions, saves and resumes, and boot logs), and `lanescape-runners-proxy` for the proxy's stored responses.
- **Secret Manager** holds the App's private key and webhook secret. Their values are added by hand, once.

What machines, disks and images exist, and what they're for, is in their names and labels; what jobs want is in GitHub; what can't be read back from either is in the bucket.

## Runner groups

GitHub gives a job any runner, in a group its repository may use, whose labels include all of the job's. A group per repository keeps one repository's job off a machine made for another's. The first time a consumer queues a job, the service creates its group, `lanescape-runners-<repository ID>`, limited to that repository, and registers that consumer's runners only there. A consumer's limits are the organization-wide defaults in `config/limits.yaml`, unless that file overrides them for the consumer by repository ID.

## Endpoints

The service is reachable from the internet, so every endpoint authenticates its caller itself:

- **The webhook** checks GitHub's HMAC signature over the body before reading it as JSON, with a size limit on the body, and ignores deliveries for any other organization.
- **The reconcile endpoint** accepts only Google-signed OIDC tokens whose audience is the service, and whose account is the scheduler's or the push subscriptions', verified as such.
- **The run binding** accepts only GitHub OIDC tokens whose audience is the service, signed by GitHub's published keys, whose repository's owner ID is the organization's, and whose run is in progress; everything it's told is checked against GitHub before it binds the run's scope (specs/0002-machines.md).
- **The job-started check and the job-finished report** accept only a machine's Compute Engine identity token in the full format, whose audience is the service and whose project, zone and instance ID are a machine the service made (specs/0002-machines.md).

The endpoints are versioned, `/v1/...`. Machines and their hooks come from the service's own base images, so they speak the version of the service that made them; a change that breaks `/v1/` comes as `/v2/` beside it, and `/v1/` goes once no machine or action uses it. The proxy's interface is versioned the same way (specs/0006-proxy.md).

## Passes

One pass runs at a time, under a lease in the bucket: an object taken with a generation precondition, valid for 10 minutes, renewed every minute during the pass with the same precondition, and released at its end. A pass has an absolute deadline of 7 minutes from its start. Every call it makes to GitHub or Google is capped by the time left, and reading the queue has a shorter deadline of its own, so a slow GitHub can't starve cleanup. A pass whose renewal fails stops changing anything at once. Cloud Run's request timeout, the scheduler's attempt deadline and the push subscriptions' acknowledgement deadlines are all 10 minutes, above the pass's deadline. A push that finds the lease held is answered with a retryable error, and Pub/Sub delivers it again with backoff; an event is acknowledged only once a pass has run after it arrived.

A pass:

1. Lists the queued jobs of every consumer's runs, the service's machines, disks and images, and its records.
2. Gives queued jobs machines, within each consumer's limits (specs/0002-machines.md).
3. Saves the work disks of jobs that asked, and makes resume images for preempted Spot jobs (specs/0003-disks.md).
4. Reruns runs that lost a Spot job to preemption, once they've finished.
5. Deletes what's finished, too old, expired or orphaned, and removes offline runner registrations named after its machines.

A failure on one machine, job or disk doesn't stop the rest of a pass. If the service itself fails, Compute Engine still stops every machine at its maximum run duration, leaving only its disks to pay for.

## Forks and contributions

A job from a pull request whose head is in another repository never runs on the service's machines, except for an approved contribution, below. The service makes no machine for any other such job, and if one takes a machine made for another job, the job-started check refuses it before its first step (specs/0002-machines.md). In a fork, or for a fork's pull request, the actions resolve every runner to its fallback, normally one of GitHub's, and read sources straight from upstream. A fork's pull request runs there, at GitHub's expense, whatever needs nothing of ours.

**Approved contributions.** A consumer may also run a contributor's pull request on the service's machines, through a `pull_request_target` workflow whose jobs name the environment `contribution`. The environment's required reviewers are the consumer's maintainers, and each push to the pull request makes a new run that waits for approval again. The service accepts a job from such a run only if GitHub records an approval of the `contribution` environment for that run, and counts it against the consumer's separate limit for contributions. Every other check still applies.

A `pull_request_target` run runs the default branch's workflow file with the base repository's privileges, so its jobs' tokens name default-branch code even when a job runs the contributor's. So:

- a job that runs the contributor's code checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, never the branch, without credentials, so the head repository must be readable without them: contributions come from public forks of public repositories;
- it has `permissions: {}`, no secrets and no `id-token`;
- it saves work disks only in that pull request's own scope (specs/0003-disks.md);
- nothing that trusts a token's `job_workflow_ref` may trust it for a `pull_request_target` run: the proxy doesn't, and a consumer's own workload identity federation must refuse those tokens by `event_name`. The service makes one narrow exception: the run binding accepts the consumer's bootstrap workflow, a reusable workflow that does nothing but run the `runners` action and that no job running a contributor's code is part of (specs/0002-machines.md);
- a workflow on the default branch never restores from GitHub's Actions cache, which a `pull_request_target` job's runtime token can write to as if it were the default branch.

Whether a contribution's results count as the pull request's required checks is the consumer's to arrange; a trusted final job can post each result as a commit status on the pull request's head commit.
