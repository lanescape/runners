# Architecture

Lanescape Runners is a GitHub Actions runner service for the repositories of the `lanescape` GitHub organization, and for no one else, on Google Cloud. A job asks for a machine by label, as it would ask GitHub or any other runner provider: `runs-on: lanescape-80vcpu-linux-amd64`. The service creates a Compute Engine machine for that job, registers a one-job runner on it, and deletes it afterwards. A job's work disk can be saved when it finishes, and later jobs start from it (specs/0003-disks.md). Jobs on any runner, the service's machines or runners outside the service, can save and restore build state through the service's cache (specs/0006-cache.md). Nothing runs while no job is queued, apart from storage. Repositories that use it are **consumers**: any repository of the organization.

The service provides compute, saved disks and the cache. It never decides what a job may do in a consumer's own cloud resources, and a job's machine has no identity that can reach them: its account has no role anywhere (specs/0002-machines.md). A job that needs a consumer's resources signs in with GitHub's OIDC token through that consumer's own workload identity federation.

## Only the `lanescape` organization

The code is public under the MIT License, so anyone may read or copy it. Only the `lanescape` organization may use the service, and it's built so that no one else can:

1. **The GitHub App is private to the organization.** It's set to install only on its owner's account, so no other organization or person can install it, and the service registers runners nowhere else.
2. **Every check is by ID.** The organization's ID is in the service's configuration; a repository is a consumer if its owner ID is that, whatever its name. Every endpoint checks the owner ID GitHub reports, so a renamed or look-alike organization never matches.
3. **Runners register only in the organization's runner groups,** one per repository, made by the service (below).
4. **Machines, saved disks and images are private to the project.** Compute Engine images are visible only within their project unless shared, and the service never shares one. The service's buckets enforce public access prevention, and the cache hands out signed URLs, each for one object, only to the jobs that may read or write it (specs/0006-cache.md). Nothing the service stores can be read from outside the organization, so no one else's reads cost it anything.
5. **Forks get nothing of the service's** (below), and the actions in `lanescape/actions` do nothing of the service's outside the organization. Anyone can reference a public action; it falls back to GitHub's runners and restores and saves nothing, and the service refuses it anyway.

A consumer's own buckets and identities are the consumer's to keep private: public access prevention on, and no sign-in for a fork's job or for a `pull_request_target` run.

## Components

All of it runs in project `lanescape-dev`, region `northamerica-northeast1`, and is defined in this repository's Terraform (specs/0004-deployment.md).

- **The service,** one Go program on Cloud Run, with GitHub's webhook, the reconcile endpoint, the run binding, the job-started check, the job-finished report and the cache's three endpoints (specs/0006-cache.md).
- **A GitHub App,** "Lanescape Runners", installed on the organization. It creates and manages a runner group per consumer, registers just-in-time runners in them, lists jobs and reruns runs, reads pull requests, and reads consumers' default-branch files, with the permissions specs/0004-deployment.md lists. Its webhook delivers `workflow_job`, `workflow_run` and `pull_request` events; of a pull request's, only its closing starts a pass.
- **Pub/Sub.** The webhook endpoint checks the delivery's signature and publishes its body to a topic, with the delivery's event as the message's attribute `event`, and a push subscription delivers it to the reconcile endpoint, which records a pull request's closing, by GitHub's `closed_at`, before acknowledging it, since GitHub no longer shows it once the pull request is reopened (specs/0003-disks.md, Expiry). A log sink publishes Compute Engine's preemption and guest-shutdown events for the service's machines to another topic, pushed to the same endpoint, which records each preemption in the bucket before acknowledging it.
- **Cloud Scheduler** calls the reconcile endpoint every 5 minutes. Events can be lost, delayed or duplicated, so a pass works from what GitHub and Compute Engine report when it runs, never from the event that started it, and the sweep catches anything an event missed.
- **Buckets:** `lanescape-runners` for the service's state (the lease, records of preemptions, saves, resumes and the cache's entries, and boot logs), and `lanescape-runners-cache` for the cache's entries themselves.
- **Secret Manager** holds the App's private key and webhook secret. Their values are added by hand, once.

What machines, disks and images exist, and what they're for, is in their names and labels; what jobs want is in GitHub; what can't be read back from either is in the bucket.

## Runner groups

GitHub gives a job any runner, in a group its repository may use, whose labels include all of the job's. A group per repository keeps one repository's job off a machine made for another's. The first time a consumer queues a job, the service creates its group, `lanescape-runners-<repository ID>`, limited to that repository, and registers that consumer's runners only there.

## Endpoints

The service is reachable from the internet, so every endpoint authenticates its caller itself:

- **The webhook** checks GitHub's HMAC signature over the body before reading it as JSON, with a size limit on the body, and ignores deliveries for any other organization.
- **The reconcile endpoint** accepts only Google-signed OIDC tokens whose audience is the service, and whose account is the scheduler's or the push subscriptions', verified as such.
- **The run binding** accepts only GitHub OIDC tokens whose audience is the service, signed by GitHub's published keys, whose repository's owner ID is the organization's, and whose run is in progress; everything it's told is checked against GitHub before it binds the run's scope (specs/0002-machines.md).
- **The cache** accepts the same tokens, only while GitHub shows the token's own job in progress, and only for a run that's bound; what it reads and writes is the binding's scope's (specs/0006-cache.md).
- **The job-started check and the job-finished report** accept only a machine's Compute Engine identity token in the full format, whose audience is the service and whose project, zone and instance ID are a machine the service made (specs/0002-machines.md).

The endpoints are versioned, `/v1/...`. Machines and their hooks come from the service's own base images, so they speak the version of the service that made them; a change that breaks `/v1/` comes as `/v2/` beside it, and `/v1/` goes once no machine or action uses it.

## Passes

One pass runs at a time, under a lease in the bucket: an object taken with a generation precondition, valid for 10 minutes, renewed every minute during the pass with the same precondition, and released at its end. A pass has an absolute deadline of 7 minutes from its start. Every call it makes to GitHub or Google is capped by the time left, and reading the queue has a shorter deadline of its own, so a slow GitHub can't starve cleanup. A pass whose renewal fails stops changing anything at once. Cloud Run's request timeout, the scheduler's attempt deadline and the push subscriptions' acknowledgement deadlines are all 10 minutes, above the pass's deadline. A push that finds the lease held is answered with a retryable error, and Pub/Sub delivers it again with backoff; an event is acknowledged only once a pass has run after it arrived.

A pass:

1. Lists the queued jobs of every consumer's runs, the service's machines, disks and images, and its records.
2. Gives queued jobs machines, oldest first, from one pool for every consumer (specs/0002-machines.md, One pool).
3. Saves the work disks of jobs that asked, and makes resume images for preempted Spot jobs (specs/0003-disks.md).
4. Reruns runs that lost a Spot job to preemption, once they've finished.
5. Deletes what's finished, too old, expired or orphaned, and removes offline runner registrations named after its machines.

A failure on one machine, job or disk doesn't stop the rest of a pass. If the service itself fails, Compute Engine still stops every machine at its maximum run duration, leaving only its disks to pay for.

## Forks and contributions

A job from a pull request whose head is in another repository never runs on the service's machines, except for an approved contribution, below. The service makes no machine for any other such job, and if one takes a machine made for another job, the job-started check refuses it before its first step (specs/0002-machines.md). In a fork, or for a fork's pull request, the actions resolve every runner to its fallback, normally one of GitHub's, and read sources straight from upstream. A fork's pull request runs there, at GitHub's expense, whatever needs nothing of ours.

**Approved contributions.** A consumer may also run a contributor's pull request on the service's machines, through a `pull_request_target` workflow whose jobs name the environment `contribution`. The environment's required reviewers are the consumer's maintainers, and each push to the pull request makes a new run that waits for approval again. The service accepts a job from such a run only if GitHub records an approval of the `contribution` environment for that run. Every other check still applies.

A `pull_request_target` run runs the default branch's workflow file with the base repository's privileges, so its jobs' tokens name default-branch code even when a job runs the contributor's. So:

- a job that runs the contributor's code checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, never the branch, without credentials, so the head repository must be readable without them: contributions come from public forks of public repositories;
- it has no secrets, and `permissions: {}`, except that a job that uses the cache, normally one on a runner outside the service, has `id-token: write` and `contents: read`, and nothing else (specs/0006-cache.md, Who may use it);
- it saves work disks and cache entries only in that pull request's own scope (specs/0003-disks.md, specs/0006-cache.md);
- nothing that trusts a token's `job_workflow_ref` may trust it for a `pull_request_target` run, and a consumer's own workload identity federation must refuse those tokens by `event_name`. A contribution's job that uses the cache can mint tokens of the base repository for any audience, so this holds for everything that trusts the consumer's tokens, not only its federation: each must refuse a `pull_request_target` run's, by `event_name`. The cache trusts nothing of such a token but its run's binding and its job. The service makes one narrow exception: the run binding accepts the consumer's bootstrap workflow, a reusable workflow that does nothing but run the `runners` action and that no job running a contributor's code is part of (specs/0002-machines.md);
- a workflow on the default branch never restores from GitHub's Actions cache, which a `pull_request_target` job's runtime token can write to as if it were the default branch.

Whether a contribution's results count as the pull request's required checks is the consumer's to arrange; a trusted final job can post each result as a commit status on the pull request's head commit.
