# Machines

## Selectors

A job asks for one label, its **selector**: a shape, optionally Spot and a saved disk, and the run's scope.

```
lanescape-<vCPUs>vcpu-linux-<amd64|arm64>[-spot][-disk-<family>-<key>]-<scope>
```

```yaml
runs-on: lanescape-80vcpu-linux-amd64-main
runs-on: lanescape-80vcpu-linux-amd64-spot-disk-build-3f9c2e7a10b4d5e6f708-pr42
```

Jobs don't write selectors by hand: the `runners` action makes them from the consumer's `.github/runners.json` and the run, and falls back to other runners outside the organization (specs/0005-actions.md).

**Shapes.** Each names the machine types that satisfy it, in order of preference, in `config/shapes.yaml`: at least that many vCPUs of that architecture, the memory the shape promises, and the size of its work disk. For example, `lanescape-80vcpu-linux-amd64` is `n4-highcpu-80`, then `c4-highcpu-96` when N4's quota or capacity is short; `lanescape-16vcpu-linux-arm64` is `c4a-highcpu-16`. Machines are made in zones `northamerica-northeast1-a`, then `-b`. Every machine boots the service's base image for its architecture (specs/0003-disks.md), on a 100 GB boot disk, and gets a work disk (below), each with 3,000 IOPS and 400 MiB/s.

**Provisioning.** Machines are standard unless the selector says `-spot`. A Spot machine costs less and can be preempted at any time. A job that asks for one must be able to pick up where it left off on a rerun, from the work disk its preempted machine left (below). A standard machine is never preempted; Compute Engine migrates it live for host maintenance.

**Saved disks.** `-disk-<family>-<key>` asks for a work disk made from a saved one (specs/0003-disks.md). `<family>` is a name the consumer chooses, of lowercase letters, digits and hyphens; `<key>` is 20 lowercase hex digits the consumer computes from whatever the disk's contents depend on.

**Scope** is where the run may read and save disks, and it's what keeps one run's disks from reaching another's machines:

| Scope | The run | Reads | Saves |
|---|---|---|---|
| `main` | A push to the default branch | Main's disks | Main's disks |
| `pr<number>` | A pull request's run, the repository's own or an approved contribution | Its pull request's disks, then main's | Its pull request's disks |
| `read` | Anything else | Main's disks | Nothing |

The service never takes a selector's scope on trust. Each run's scope is bound once, in the bucket, and every decision about the run's jobs uses that binding; a job of a run with no binding gets no machine and no disk. The run's first job binds it, through the `runners` action, which sends the service its GitHub OIDC token (specs/0005-actions.md). The binding is created only if absent, so a later job of the same run can't change it. It comes from what GitHub signed in that token, never from what the job says, checked against GitHub's records:

- **`main`** only for a push whose token's `ref` is `refs/heads/<default branch>` and `ref_type` is `branch`, and whose head commit is on the default branch, by GitHub's comparison of the two. A tag or another branch named like the default branch fails both.
- **`pr<number>`** for a `pull_request` run only from its token's `ref`, `refs/pull/<number>/merge`, which GitHub signs and the workflow can't change; and only if GitHub's pull request of that number targets this repository, and its head repository and head commit are the run's. A same-repository pull request's author controls its workflow, so nothing the workflow sends is used.
- **`pr<number>`** for a `pull_request_target` run, whose token's `ref` is the base branch's and names no pull request, only from a call by the consumer's bootstrap workflow: a reusable workflow on the default branch, whose `job_workflow_ref` the consumer names in `.github/runners.json`, and which does nothing but run the `runners` action. A `pull_request_target` run runs only the default branch's workflow files, so the contributor can't change that workflow or what it sends, and a job that runs the contributor's code is a job of the calling workflow, whose `job_workflow_ref` is the caller's, never the bootstrap's. The pull request it names must target this repository and have the event's head repository, with the event's head commit among its commits.
- **`read`** for anything else, and for a pull request run whose head commit and repository are the head of more than one open pull request.

If GitHub can't be reached, or limits the service's requests, while a binding is being checked, no binding is made, and the action gets a retryable error and tries again; only a confirmed answer binds a run. The action's outputs come from the binding the service returns. The service makes no machine for a job whose selector's scope isn't its run's binding, and the job-started check refuses one. A machine's work disk may hold a scope's data, so a job of one scope must never run on a machine made for another's, even with the same shape: that's why every selector carries its scope, and why a selector is one label. GitHub gives a job any runner whose labels include all of the job's, so a runner registered with `[shape, scope]` as two labels would match a job that asked for the shape alone.

A runner carries exactly one selector, the one it was made for, and GitHub's own `self-hosted`, `linux` and architecture labels. Labels compare without regard to case, as GitHub's do, so the service works with a selector in lowercase throughout. A selector can be longer than a Compute Engine label value may be, so a machine's labels hold the first 20 hex digits of the lowercase selector's SHA-256, and its metadata holds the selector itself.

## Network

Machines join the service's network, `lanescape-runners`. It has one firewall rule: it lets the service's machines reach the proxy's git mirror at its internal address on port 443, by their accounts, which is how Google matches a rule's source account (specs/0006-proxy.md). Otherwise its implicit rules apply: nothing reaches a machine, not another machine, not the project's other machines, not SSH from outside. Each still reaches out through its own external address. Deploying refuses a change that adds any other rule.

## A machine's life

Every machine runs the **supervisor**, a small program of the service's own on the base image. It registers the machine's one-job GitHub runner, starts it, waits for it to exit, then sends the service its **report**: that the job is done, and whether the job asked to save its work disk. Then it powers the machine off. It runs outside every job, as its own user with its own environment, and acts only after the runner has exited, by which time GitHub has the job's result. That's why it, and not the runner's job-completed hook, ends a machine's life: that hook runs before the runner has reported the result, and inside the job's environment, which the job can change.

1. A job is queued. If its run's binding allows it, the service creates a machine for it, unless as many machines are already being made or idle for that consumer and selector as there are such jobs queued: `lsr-<repository ID>-<random>`, labelled with the repository ID, its selector's hash and its creation time. Its account is `lanescape-runners-machine`, which has no role anywhere (below). It has a boot disk and no work disk yet. A supervisor, the service's own program on the base image, registers a just-in-time runner with that selector in the consumer's runner group, with the runner's own working directory on the boot disk, starts it, and waits for it.
2. GitHub hands the runner any queued job of that consumer whose labels it matches. It need not be the job the machine was made for. Before the job's first step, the runner's job-started hook asks the service whether the job may run here (below). If the service allows it, it makes the job's work disk, attaches it and answers; the hook mounts it at `/work` and returns. If the service refuses, it stops the machine itself at once, and the hook, which gets no answer it can use, never returns. A machine is never given a disk before its actual job is known, so a refused job, or one whose hook was tampered with, finds no work disk and no identity, only a machine about to stop. The service cancels the run of a job it refused for coming from a fork or for claiming a scope that isn't its run's, and makes another machine for any job still queued.
3. When the runner has finished its job and reported the result to GitHub, it exits, and the supervisor, not a hook, reports to the service, with whether the job asked to save its work disk (specs/0005-actions.md), and powers the machine off. The report, not GitHub's word that the job finished, is what starts the machine's cleanup: GitHub can show the job finished a moment before the report arrives, and only the report says whether the job asked to save. A report normally arrives seconds after the job ends, and the service acts on it at once. The service records every report, saving or not, and saves the disk if GitHub's conclusion for the job is success, the job asked, and its scope saves (specs/0003-disks.md). If a finished job's report never arrives, because its machine or supervisor died, the service waits 10 minutes, then goes on as if the job asked for no save. Then it saves the boot log to its bucket as `logs/<machine>-<created>.txt` and deletes the machine and its disks.

The supervisor runs outside every job, as its own user, with its own environment and paths; nothing a job sets, such as `GITHUB_PATH`, `GITHUB_ENV` or files it leaves, reaches it. The job-started hook does run in the job's environment, before any step, so it decides nothing: what it asks, the service answers from GitHub's records, and what it would enforce, the service enforces by stopping the machine.

**The job-started check.** The hook asks the service, signed with the machine's identity token: a Compute Engine token in the full format, whose audience is the service and whose project, zone and instance ID the service checks against the machine it made. The service asks GitHub which job is running on that machine's runner, by the job's `runner_id` and `runner_name`, never what the machine says, retrying for a short while until GitHub shows the assignment, and records the binding of job to machine. It allows the job only if:

- the job's repository is the machine's consumer, and its owner is the organization;
- its run isn't from a fork's pull request, unless it's an approved contribution (specs/0001-architecture.md);
- its labels hold exactly one selector, the machine's, with nothing else but GitHub's own labels that the runner has;
- the selector's scope is the run's binding.

Then it picks the job's work disk for that job: its run's resume image for that selector and job, or the run's recorded restore choice for that selector (specs/0003-disks.md). Disks may be prepared ahead for the job a machine was made for, and are attached only if they're what the admitted job should have. Every work disk is recorded in the bucket, with its machine, its job and whether it's being made, made or attached, before it's created; a retried admission carries on with the disk already recorded, and cleanup never deletes a recorded disk that's being made or waiting to be attached.

A runner can be taken by a job of the same consumer that was never meant for it: a fork's pull request that names one of the service's selectors after a maintainer approved its run, a job whose selector claims a scope that isn't its run's, or a job that asked only for `self-hosted`. This check is what keeps that job's steps from running. GitHub runs a fork's pull request workflows only once a maintainer approves them, the organization's setting for every outside contributor, which the service checks at deploy and refuses to run without. When the service refuses a fork's job, it records the refusal and cancels the run, retrying until GitHub shows the run finished, and force-cancels it if an ordinary cancellation hasn't finished it within a few minutes. Until then it makes no further machine for that run's selectors. A first wave of the run's jobs may still each take a machine before the first refusal; that cost is bounded by the consumer's limit, and is a maintainer's approval of a fork's workflow gone wrong.

**The machine's account** exists only because Compute Engine issues identity tokens only to machines with an account. It has no role on any resource, so its access tokens reach nothing that isn't open to anyone signed in to Google, and no consumer may grant it one. A job that mints its machine's identity token can only ask about the job running on its own machine, which is itself, and read through the proxy as its consumer (specs/0006-proxy.md).

| What a pass finds | What it does |
|---|---|
| The supervisor's report is recorded | Saves the work disk if the job succeeded, asked and its scope saves; saves the boot log; deletes the machine and its disks |
| GitHub shows the job finished, but no report has arrived after 10 minutes | Goes on as if the report asked for no save |
| A Spot machine was preempted, or might have been | Records it, makes a resume image of its work disk if its scope saves (below), then deletes the machine and its disks |
| The machine is past 5 hours | Deletes it, even mid-job, without saving anything. Compute Engine itself stops every machine at 6 hours |
| The machine stopped before its job started | Deletes it; another is made while the job is still queued, up to an hour from the job's first machine, after which the service leaves the job queued and logs why |
| The machine hasn't picked up a job after 20 minutes | Deletes it, with the same hour's limit |
| A disk attached to nothing, and not recorded as being made or waiting to be attached | Deletes it |
| An offline runner registration whose machine is gone | Removes it, if it's in one of the service's groups and named after one of its machines |

A consumer has two limits, counted apart: machines for its jobs at once, and machines for approved contributions at once. A job beyond its limit waits queued until there's room. Spending as a whole is watched, not capped: the project's budget alert tells the maintainer, who acts on it. Quota errors, stockouts and the zone order are the service's to handle, not the job's.

## The work disk

Every machine gets a second disk, mounted at `/work` and owned by the runner's user, which holds only what jobs put there: toolchains, source trees, build output, whatever is slow to make again. Its size is the shape's, or its saved disk's if that's larger. The runner's own working directory, where checkouts and the runner's files go, is on the boot disk, so tokens that tools store beside a checkout never reach `/work`. A job that handles long-lived secrets doesn't ask to save its disk.

## Preemption

Only Spot machines are preempted. Preemption is learned from either of two sources, whichever comes first: Compute Engine's event, which the reconcile endpoint records in the bucket by instance ID before acknowledging it, and the zone operations of type `compute.instances.preempted`, whose `targetId` is the instance's numeric ID. Google keeps a finished operation for as little as an hour, so the recorded event is what survives an outage of the service.

Before a stopped Spot machine is deleted, the service records in the bucket its instance ID, zone, the GitHub job ID and runner ID of the job that was on it, the job's name, its run and attempt, its selector, and whether it was preempted: `yes`, or `unknown` while there's no evidence. If its scope saves disks, it makes a **resume image** of the machine's work disk (specs/0003-disks.md), then deletes the machine and its disks. Passes go on looking for evidence for each `unknown` record by its instance ID after the machine is gone. A resume image is kept for a day after its machine stopped, and then goes whatever the record says. That day is the limit on recovery, not a finding that the machine wasn't preempted: a run whose job was lost and never shown preempted isn't rerun.

**Reruns.** Once a run has finished, if any of its failed jobs was a Spot job shown preempted, the service asks GitHub to rerun the run's failed jobs, rather than each lost job and its dependents, for up to 3 attempts of the run in all, counted in the bucket. That reruns jobs that failed for other reasons too, and anything that depends on them; it keeps one rerun per run attempt, where rerunning lost jobs one at a time would start a new attempt with each.

**Resuming.** A resume image belongs to its run and to the job's name. When a rerun's job is admitted, the job-started check gives it its resume image if one is eligible: in both the attempt that was preempted and the one starting, exactly one job has that name, so a name a matrix shares with its siblings never resumes. The check claims the image for the starting job's ID in the bucket, created only if absent, and makes the work disk from it. Any other job, of the same run or another, never gets it. Images are global, so a rerun's machine can be in any zone. A resume image goes once its job has finished.

Either way the job runs the same steps: a step that finds its state in `/work` carries on from it, and one that doesn't starts as it would on a new disk. A preempted disk is in whatever state its machine left it mid-write, as after a power cut; a job that asks for Spot must cope with that. In the `read` scope, nothing is saved, so a preempted job is rerun on a disk made as for any job.
