# Architecture

Lanescape Runners is a GitHub Actions runner service for the repositories of the `lanescape` GitHub organization, and for no one else, on Google Cloud. A job asks for a machine by label, as it would ask GitHub or any other runner provider, naming only its shape: `runs-on: lanescape-80vcpu-linux-amd64`. The service creates a Compute Engine machine for that job, booted from the service's own base image, registers a one-job runner on it, and deletes it afterwards. Machines are general: a job sets its machine up in its own steps, from its own commit, with passwordless sudo (specs/0002-machines.md, Setting up a machine), and can ask in a step for a work disk, which can be saved when it finishes, so that later jobs start from it (specs/0003-disks.md). The service builds and stores no images of consumers'. Nothing runs before a workflow's jobs to set this up: the service binds each run's scope at its first job's check, from what GitHub told the job's runner, confirmed against GitHub's own records. Nothing runs while no job is queued, apart from storage. Repositories that use it are **consumers**: any repository of the organization.

The service provides compute and saved disks. It never decides what a job may do in a consumer's own cloud resources, and a job's machine has no identity that can reach them: its account has no role anywhere (specs/0002-machines.md). A job that needs a consumer's resources signs in with GitHub's OIDC token through that consumer's own workload identity federation.

## Only the `lanescape` organization

The code is public under the MIT License, so anyone may read or copy it. Only the `lanescape` organization may use the service, and it's built so that no one else can:

1. **The GitHub App is private to the organization.** It's set to install only on its owner's account, so no other organization or person can install it, and the service registers runners nowhere else.
2. **Every check is by ID.** The organization's ID is in the service's configuration; a repository is a consumer if its owner ID is that, whatever its name. Every endpoint checks the owner ID GitHub reports, so a renamed or look-alike organization never matches.
3. **Runners register only in the organization's runner groups,** one per repository, made by the service (below).
4. **Machines and saved disks are private to the project.** Compute Engine images are visible only within their project unless shared, and the service never shares one. The service's bucket enforces public access prevention. Nothing the service stores can be read from outside the organization, so no one else's reads cost it anything.
5. **Forks get nothing of the service's** (below), and the actions in `lanescape/actions` do nothing of the service's outside the organization. Anyone can reference a public action; outside the organization it does nothing of the service's, and the service refuses it anyway.

A consumer's own buckets and identities are the consumer's to keep private: public access prevention on, no sign-in for a fork's job, and for a `pull_request_target` run's job only what the consumer means a contribution to have (Forks and contributions, below).

## Components

All of it runs in project `lanescape-dev`, region `northamerica-northeast1`, and is defined in this repository's Terraform (specs/0004-deployment.md).

- **The service,** one Go program on Cloud Run, with GitHub's webhook, the reconcile endpoint, the disk and save requests, the job-started check and the job-finished report.
- **A GitHub App,** "Lanescape Runners", installed on the organization. It creates and manages a runner group per consumer, registers just-in-time runners in them, lists jobs and reruns runs, reads runs, pull requests and environments' approvals, and reads consumers' files: `.github/runners.json` from the default branch, and from a run's commit, that file and the files its disks depend on, with the permissions specs/0004-deployment.md lists. Its webhook delivers `workflow_job`, `workflow_run` and `pull_request` events; of a pull request's, only its closing starts a pass.
- **Pub/Sub.** The webhook endpoint checks the delivery's signature and publishes its body to a topic, with the delivery's event as the message's attribute `event`, and a push subscription delivers it to the reconcile endpoint, which records a pull request's closing, by GitHub's `closed_at`, before acknowledging it, since GitHub no longer shows it once the pull request is reopened (specs/0003-disks.md, Expiry). A log sink publishes Compute Engine's preemption and guest-shutdown events for the service's machines to another topic, pushed to the same endpoint, which records each preemption in the bucket before acknowledging it.
- **Cloud Scheduler** calls the reconcile endpoint every 5 minutes. Events can be lost, delayed or duplicated, so a pass works from what GitHub and Compute Engine report when it runs, never from the event that started it, and the sweep catches anything an event missed.
- **A bucket,** `lanescape-runners`, for the service's state: the lease, runs' bindings, records of preemptions, saves, resumes, saved disks' images and base images' builds, and boot logs.
- **Secret Manager** holds the App's private key and webhook secret. Their values are added by hand, once.

What machines, disks and Compute Engine images exist, and what they're for, is in their names and labels; what jobs want is in GitHub; what can't be read back from either is in the bucket.

## Runner groups

GitHub gives a job any runner, in a group its repository may use, whose labels include all of the job's. A group per repository keeps one repository's job off a machine made for another's, which its check would refuse. The first time a consumer queues a job, the service creates its group, `lanescape-runners-<repository ID>`, limited to that repository, and registers that consumer's runners there and nowhere else. Every machine boots the same base image, with the service's own runner and hook, and holds nothing of any run's until its job is admitted, so no machine needs a group of a scope's own: whichever of the repository's jobs takes it, the job-started check decides (specs/0002-machines.md). A job that asks for the group by name, `runs-on: {group: ...}`, with no selector, may be given one of its runners, and its check refuses it, since its labels aren't a selector.

## Endpoints

The service is reachable from the internet, so every endpoint authenticates its caller itself:

- **The webhook** checks GitHub's HMAC signature over the body before reading it as JSON, with a size limit on the body, and ignores deliveries for any other organization.
- **The reconcile endpoint** accepts only Google-signed OIDC tokens whose audience is the service, and whose account is the scheduler's or the push subscriptions', verified as such.
- **The disk and save requests, the job-started check and the job-finished report** accept only a machine's Compute Engine identity token in the full format, whose audience is the service and whose project, zone and instance ID are a machine the service made (specs/0002-machines.md). A disk or save request is answered only for the job the service admitted on that machine, while it runs, and its run. A job-started check binds its job's run only from that machine's first check, and only as GitHub's records confirm (specs/0002-machines.md, Binding a run). No endpoint accepts a GitHub token.

Neither the service nor any program of its own on a machine ever logs a credential: no token it mints or is given, no JIT runner configuration, identity token, upload session's URI or signed URL; nor does the service write one to its bucket.

The endpoints are versioned, `/v1/...`. Machines and their programs come from the service's own base images, so they speak the version of the service that made them; a change that breaks `/v1/` comes as `/v2/` beside it, and `/v1/` goes once no machine or action uses it.

## Passes

One pass runs at a time, under a lease in the bucket: an object taken with a generation precondition, valid for 10 minutes, renewed every minute during the pass with the same precondition, and released at its end. A pass has an absolute deadline of 7 minutes from its start. Every call it makes to GitHub or Google is capped by the time left, and reading the queue has a shorter deadline of its own, so a slow GitHub can't starve cleanup. A pass whose renewal fails stops changing anything at once. Cloud Run's request timeout, the scheduler's attempt deadline and the push subscriptions' acknowledgement deadlines are all 10 minutes, above the pass's deadline. A push that finds the lease held is answered with a retryable error, and Pub/Sub delivers it again with backoff; an event is acknowledged only once a pass has run after it arrived.

A pass:

1. Lists the queued jobs of every consumer's runs, the service's machines, disks and Compute Engine images, and its records.
2. Stops every machine past its age limit, before it changes anything else (specs/0002-machines.md, The age limit).
3. Moves each architecture's base image for the current key on, if it isn't ready, and deletes base images it no longer needs (specs/0003-disks.md, Making a base image), before queued jobs take what capacity there is. A builder that finds no capacity or quota holds its machine families' quota as an older job does (specs/0002-machines.md, One pool): no job gets a machine of those families that pass, so that freed quota builds up for the builder, and a busy pool never starves it.
4. Gives queued jobs machines, in one order, oldest first, from one pool for every consumer (specs/0002-machines.md, One pool).
5. Saves the work disks of jobs that asked, and makes resume images for jobs lost to a preemption or to the age limit (specs/0003-disks.md).
6. Reruns runs that lost a job to a preemption or to the age limit, once they've finished.
7. Deletes what's finished, too old, expired or orphaned, and removes offline runner registrations named after its machines.

A failure on one machine, job or disk doesn't stop the rest of a pass. If the service itself fails, Compute Engine still stops every machine at its maximum run duration, leaving only its disks to pay for, and deletes a running base image's builder at its own.

## Forks and contributions

A job from a pull request whose head is in another repository is never given one of the service's machines, except for an approved contribution, below. The service makes no machine for any other such job, and if one takes a machine made for another job, the job-started check refuses it before its first step (specs/0002-machines.md). In a fork, no runner carries the service's selectors, so a consumer's workflow skips the jobs that name them there, or picks another label for them. It tells both cases apart by expressions: in a fork's own runs, `github.repository_owner_id` isn't the organization's; in a fork's `pull_request` run into the consumer, which runs in the consumer's repository, the event's head repository's ID isn't `github.repository_id`; an approved contribution's `pull_request_target` run has a fork's head too, and does use the service's machines. The service makes no machine for a fork's `pull_request` job that names a selector, and cancels its run, so it doesn't wait for a machine that never comes. A fork's pull request runs, at GitHub's expense, whatever needs nothing of ours.

**Approved contributions.** A consumer may also run a contributor's pull request on the service's machines, through a `pull_request_target` workflow with a contribution job: one that names the environment `contribution`, runs only the default branch's code, and that every other job of the run on the service's machines needs. The environment's required reviewers are the consumer's maintainers, and each push to the pull request makes a new run that waits for approval again. The contribution job's check binds the run to the event's pull request, and only if GitHub records an approval of the `contribution` environment for that run (specs/0002-machines.md, Binding a run). Every other check still applies. Until it's bound, the run's jobs are admitted only on the smallest shape, standard, with no work disk, which is all a `pull_request_target` workflow that runs only the default branch's code needs.

A `pull_request_target` run runs the default branch's workflow file with the base repository's privileges, so its jobs' tokens name default-branch code even when a job runs the contributor's. So:

- a job that runs the contributor's code checks out exactly `github.event.pull_request.head.sha`, the commit the approved run was made for, never the branch, without credentials, so the head repository must be readable without them: contributions come from public forks of public repositories;
- it has no secrets, and `permissions: {}`, except that a job that must sign in to the consumer's own cloud resources, through the consumer's own identity provider, may have `id-token: write`, and nothing else. That lets the contributor's code mint tokens of the base repository, for any audience, while its job runs;
- it never names the `contribution` environment, which only the contribution job does, so one approval admits the run, and the contribution job, which runs no contributor code, is what every other job waits for;
- it restores work disks along its pull request's restore chain, and saves them only in that pull request's own scope (specs/0003-disks.md), and sets its machine up from the contribution's head commit, in its own steps, as any job sets its machine up from its own commit (specs/0002-machines.md, Setting up a machine). A job of the run that runs only the default branch's code, such as a trusted final job, never runs the contribution's setup, which is the contributor's code;
- nothing that trusts a token's `job_workflow_ref` or `ref` may trust them for a `pull_request_target` run, whose tokens name the default branch's workflow and ref even in a job that runs the contributor's code. Every party that trusts the consumer's tokens, the consumer's own identity provider included, tells those tokens apart by `event_name`, and gives one only what the consumer means a contribution to have, never what it gives the default branch's runs; a party that can't refuses them. The service takes no GitHub token at all: it binds a run from what GitHub's runner was told, on a fresh machine before any step, and confirms it against GitHub's records, so a contributor's token can bind nothing, and a `pull_request_target` run binds only its event's pull request, once a maintainer has approved the run (specs/0002-machines.md, Binding a run);
- a workflow on the default branch never restores from GitHub's Actions cache, which a `pull_request_target` job's runtime token can write to as if it were the default branch.

Whether a contribution's results count as the pull request's required checks is the consumer's to arrange; a trusted final job can post each result as a commit status on the pull request's head commit.
