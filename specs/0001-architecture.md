# Architecture

Lanescape Runners is a GitHub Actions runner service for the `lanescape` organization's repositories, on Google Cloud. A job asks for a machine by label, as it would ask Blacksmith or GitHub: `runs-on: lanescape-80vcpu-linux-amd64`. The service creates a Compute Engine machine for a job, registers a one-job runner on it, and deletes it afterwards. Nothing runs and nothing is billed while no job is queued, apart from storage. Repositories that use it are **consumers**.

The service provides compute. It never decides what a job may do in a consumer's cloud resources, and a job's machine has no identity that can reach them: its account has no role anywhere (specs/0002-machines.md). Its one exception is an image builder, which runs a consumer's own default-branch code with an account the consumer chose for it (specs/0003-images.md). Machine shapes, the images machines boot from, and recovering from Spot preemption are what it abstracts (specs/0002-machines.md).

## Components

All of it runs in project `lanescape-dev`, region `northamerica-northeast1`, and is defined in this repository's Terraform (specs/0004-deployment.md).

- **The service,** one Go program on Cloud Run, with four endpoints: GitHub's webhook, the reconcile endpoint, the job-started check and the images API.
- **A GitHub App,** "Lanescape Runners", installed on each consumer. It registers just-in-time runners in an organization runner group per consumer, lists jobs and reruns them, and reads a consumer's files for image definitions. Its webhook delivers `workflow_job` and `workflow_run` events.
- **Pub/Sub.** The webhook endpoint checks the delivery's signature and publishes it to a topic, and a push subscription delivers it to the reconcile endpoint. A log sink publishes Compute Engine's preemption and guest-shutdown events for the service's machines to another topic, pushed to the same endpoint. Either way the service reacts at once instead of on its next sweep.
- **Cloud Scheduler** calls the reconcile endpoint every 5 minutes. Events can be lost, delayed or duplicated, so a pass works from what GitHub and Compute Engine report when it runs, never from the event that started it, and the sweep catches anything an event missed.
- **A bucket,** `lanescape-runners`, for the service's own state: the lease, image build records and machines' boot logs. Consumers get no access to it.
- **Secret Manager** holds the App's private key and webhook secret. Their values are added by hand, once.

What machines, disks and images exist, and what they're for, is in their names and labels; what jobs want is in GitHub; what can't be read back from either, such as image build attempts and kept disks' jobs, is in the bucket.

## Endpoints

The service is reachable from the internet, so every endpoint authenticates its caller itself:

- **The webhook** checks GitHub's HMAC signature over the body before reading it as JSON, with a size limit on the body.
- **The reconcile endpoint** accepts only Google-signed OIDC tokens whose audience is the service, and whose account is the scheduler's or the push subscriptions', verified as such.
- **The job-started check** accepts only a machine's Compute Engine identity token (specs/0002-machines.md).
- **The images API** accepts only GitHub OIDC tokens whose audience is the service, signed by GitHub's published keys, whose repository and owner IDs are a consumer's, and whose `job_workflow_ref` is a workflow the consumer allows, on its default branch, such as `<owner>/<repo>/.github/workflows/images.yml@refs/heads/main`. A reusable workflow called that way has that `job_workflow_ref` whatever workflow calls it, so a consumer's pull request can ask for an image without being able to change what the request runs. The service also refuses a token whose run, looked up by its `run_id`, is from a fork's pull request, whatever the consumer's settings for fork tokens. The consumer is taken from the token alone, and every image, build and log the API names or returns must be that consumer's; another's reads as not found.

## Passes

One pass runs at a time, under a lease in the bucket: an object taken with a generation precondition, valid for 10 minutes, renewed every minute during the pass with the same precondition, and released at its end. A pass has an absolute deadline of 7 minutes from its start. Every call it makes to GitHub or Google is capped by the time left, and reading the queue has a shorter deadline of its own, so a slow GitHub can't starve cleanup. A pass whose renewal fails stops changing anything at once. Cloud Run's request timeout, the scheduler's attempt deadline and the push subscriptions' acknowledgement deadlines are all 10 minutes, above the pass's deadline. A push that finds the lease held is answered with a retryable error, and Pub/Sub delivers it again with backoff; an event is acknowledged only once a pass has run after it arrived.

A pass:

1. Lists the queued jobs of every consumer's runs, the service's machines, disks and images, and its build records.
2. Gives queued jobs machines, within each consumer's limit (specs/0002-machines.md).
3. Advances image builds (specs/0003-images.md).
4. Keeps the work disks of preempted machines, and reruns jobs lost to preemption once their run has finished.
5. Deletes what's finished, too old or orphaned, and removes offline runner registrations named after its machines.

A failure on one machine, job or image doesn't stop the rest of a pass. If the service itself fails, Compute Engine still stops every machine at its maximum run duration, leaving only its disks to pay for.

## Consumers

A consumer is listed in `config/consumers.yaml`, reviewed like any other change here:

- its repository and its runner group, which is limited to that repository;
- its namespace, a short name that prefixes its machines, disks and images, such as `app`;
- the labels it may use, how many machines its jobs may have at once, and how many image builders;
- the workflow allowed to request images, and where its image definitions are (specs/0003-images.md);
- the accounts, its own, that the service may attach to its image builders, and only to those.

A consumer's own cloud resources, such as its buckets, caches and the identities its jobs sign in as, are the consumer's, defined in its own repository. A job signs in to them with GitHub's OIDC token through the consumer's workload identity federation, which decides what a token from a given repository, workflow, branch and event may do.

## Forks

A job from a pull request whose head is in another repository never runs on the service's machines: the service makes none for it, and if it takes a machine made for another job, the job-started check refuses it before its first step (specs/0002-machines.md). It's left queued, or fails with its machine. A consumer may later opt into fork machines, with their own limits.

Forks run on GitHub's runners instead, through labels chosen by expression:

```yaml
runs-on: ${{ (github.event.pull_request.head.repo.full_name || github.repository) == github.repository && vars.RUNNER_LARGE_LINUX || 'ubuntu-24.04' }}
```

The organization sets `RUNNER_LARGE_LINUX` and its siblings. A fork's own pushes read the fork's variables instead, so a fork picks its own runners or gets GitHub's. The images API refuses runs from a fork's pull request whether or not GitHub gives them an OIDC token, which depends on a repository's settings and the event.
