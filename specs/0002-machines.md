# Machines

## Labels

A job asks for one label, naming a shape and optionally an image:

```yaml
runs-on: lanescape-80vcpu-linux-amd64
runs-on: lanescape-80vcpu-linux-amd64-image-lsr-app-build-3f9c2e7a10b4d5e6f708
```

A shape is `lanescape-<vCPUs>vcpu-linux-<amd64|arm64>`, with `-ondemand` to skip Spot. Each names the machine types that satisfy it, in order of preference, in `config/shapes.yaml`: at least that many vCPUs of that architecture, and the memory and disk the shape promises. For example, `lanescape-80vcpu-linux-amd64` is `n4-highcpu-80`, then `c4-highcpu-96` when N4's quota or capacity is short; `lanescape-16vcpu-linux-arm64` is `c4a-highcpu-16`. Every machine gets a boot disk of 100 GB, or its image's size if that's larger, and a work disk (below), with 3,000 IOPS and 400 MiB/s, in zones `northamerica-northeast1-a`, then `-b`. Machines are Spot, falling back to on-demand when there's no Spot capacity.

Without an image, a machine boots the service's base image for its architecture (specs/0003-images.md). With `-image-<name>`, it boots that image, which must be one of the job's consumer's own, ready, and for the shape's architecture. A job asking for a label the consumer isn't allowed, or for an image that doesn't exist, gets no machine, and the service says why in its log and leaves the job queued.

Shape and image are one label, the job's **selector**, because GitHub gives a job any runner whose labels include all of the job's. A runner registered with `[shape, image]` would match a job that asked for the shape alone and expected the base image. A runner carries exactly one selector, the one it was made for, and GitHub's own `self-hosted`, `linux` and architecture labels. Labels compare without regard to case, as GitHub's do, so the service works with a selector in lowercase throughout. A selector can be longer than a Compute Engine label value may be, so a machine's labels hold the first 20 hex digits of the lowercase selector's SHA-256, and its metadata holds the selector itself.

Machines join the service's network, `lanescape-runners`, which has no firewall rules, so its implicit ones apply: nothing reaches a machine, not another machine, not the project's other machines, not SSH from outside. Each still reaches out through its own external address. Deploying refuses a change that adds a rule to it.

## A machine's life

1. A job is queued. The service creates a machine for it, unless as many machines are already being made or idle for that consumer and label as there are such jobs queued: `lsr-<namespace>-<random>`, labelled with the consumer, its selector's hash and its creation time. Its account is `lanescape-runners-machine`, which has no role anywhere (below). Its startup script, which belongs to the service, registers a just-in-time runner with that selector in the consumer's runner group, and runs one job.
2. GitHub hands the runner any queued job of that consumer whose labels it matches; it need not be the job the machine was made for, and needn't be, since every machine for a selector is the same. Before the job's first step, the runner's job-started hook asks the service whether the job may run here (below). If not, or if it can't get an answer, the hook never returns: it stops the runner and powers the machine off, and if powering off fails it keeps the runner stopped, so none of the job's steps runs, not even one with `if: always()`. That job fails with its lost runner; the service makes another machine for any job still queued.
3. When the job finishes, the runner exits. The service saves the boot log to its bucket as `logs/<machine>-<created>.txt` and deletes the machine and its disks, unless the work disk is kept for a rerun (below).

**The job-started check.** The hook asks the service, signed with the machine's identity token: a Compute Engine token in the full format, whose audience is the service and whose project, zone and instance ID the service checks against the machine it made. The service asks GitHub which job is running on that machine's runner, never what the machine says, and allows it only if the job's repository is the machine's consumer, its run isn't from a fork's pull request (by the run's head repository), and its labels hold exactly one selector, the machine's, with nothing else but GitHub's own labels that the runner has. A runner can be taken by a job of the same consumer that was never meant for it: a fork's pull request that names the service's selector after a maintainer approved its run, or a job that asked only for `self-hosted`. This check is what keeps that job's steps from running.

**The machine's account** exists only because Compute Engine issues identity tokens only to machines with an account. It has no role on any resource, so its access tokens reach nothing that isn't open to anyone signed in to Google, and no consumer may grant it one. A job that mints its machine's identity token can only ask about the job running on its own machine, which is itself.

| What a pass finds | What it does |
|---|---|
| The job finished, and the machine wasn't preempted | Saves the boot log, and deletes the machine and its disks |
| The machine was preempted, or might have been | Records it and keeps its work disk (below), then deletes the machine |
| The machine is past 5 hours | Deletes it, even mid-job. Compute Engine itself stops every machine at 6 hours |
| The machine stopped before its job started | Deletes it; another is made while the job is still queued, up to an hour from the job's first machine, after which the service leaves the job queued and logs why |
| The machine hasn't picked up a job after 20 minutes | Deletes it, with the same hour's limit |
| A kept disk whose rerun has finished, or unused for a day | Deletes it |
| A disk attached to nothing that isn't kept | Deletes it |
| An offline runner registration whose machine is gone | Removes it, if it's in a consumer's group and named after one of the service's machines |

A consumer has two limits: machines for its jobs at once, and image builders at once (specs/0003-images.md). They're counted apart, so jobs waiting for an image, wherever they run, can never take the room its builder needs. A job beyond its limit waits queued until there's room, and so does a build. Spending as a whole is watched, not capped: the project's budget alert tells the maintainer, who acts on it. Quota errors, stockouts and the zone order are the service's to handle, not the job's.

## The work disk and preemption

Every machine gets a second disk, mounted at `/work` and owned by the runner's user, for whatever a job wants to keep if the machine is lost, such as a build's output. A consumer's jobs put their slow-to-rebuild state there.

**Preemption** is learned from either of two sources, whichever comes first: Compute Engine's event, which the reconcile endpoint records in the bucket by instance ID before acknowledging it, and the zone operations of type `compute.instances.preempted`, whose `targetId` is the instance's numeric ID. Google keeps a finished operation for as little as an hour, so the recorded event is what survives an outage of the service. Before a stopped machine is deleted, the service records in the bucket its instance ID, zone, the GitHub job ID and runner ID of the job that was on it, the job's name, its run and attempt, and whether it was preempted: `yes`, or `unknown` while no operation has been found. Its work disk is kept, labelled `kept`. Passes go on looking for either kind of evidence for each `unknown` record by its instance ID after the machine is gone. A record stays `unknown` until evidence turns up; its disk is kept for a day after the machine stopped, as every kept disk is, and then goes whatever the record says. That day is the limit on recovery, not a finding that the machine wasn't preempted: a run whose job was lost and never shown preempted isn't rerun.

**Reruns.** Once a run has finished, if any of its failed jobs was preempted, the service chooses to ask GitHub to rerun the run's failed jobs, rather than each lost job and its dependents, for up to 3 attempts of the run in all, counted in the bucket. That reruns jobs that failed for other reasons too, and anything that depends on them; it keeps one rerun per run attempt, where rerunning lost jobs one at a time would start a new attempt with each. Reruns are queued like any job, and the service makes their machines in their kept disks' zones.

**Restoring a disk is best effort.** When a rerun starts, the job-started check also looks for its kept disk. It's eligible only if, in both the attempt that was preempted and the one starting, exactly one job has that name, so a name a matrix shares with its siblings never restores; and only if the machine is in the disk's zone. The service claims the disk for the starting job's ID in the bucket, created only if absent, attaches it, and the hook mounts it at `/work` in place of the empty one. Otherwise nothing is attached. Either way the job runs the same steps: a step that finds its state in `/work` carries on from it, and one that doesn't starts as it always would.
