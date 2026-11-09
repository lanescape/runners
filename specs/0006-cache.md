# Cache

Jobs save and restore build state through the service's cache, from any runner: the service's own machines, and runners outside the service. A cache **entry** is one object a job packs, such as an archive of a build tree, in a **store**, named by family and key as saved disks are, and scoped exactly as they are: a job reads only its run's restore chain, and writes only its run's own scope (specs/0002-machines.md, Scope). Saved disks are still how the service's own machines start warm; the cache is how runners outside the service do, approved contributions included, and how any job keeps state apart from its work disk.

The service never holds an entry's bytes, and no bytes leave the region. It decides who may read and write which entry. For its own machines, it hands out short-lived signed URLs for one object at a time in its bucket, `lanescape-runners-cache`; the bytes pass between the job and Cloud Storage, never through Cloud Run, whose requests are limited to 32 MiB. For every other runner it keeps only an index. The interface's requests and answers are in specs/0005-interface.md.

## Stores

An entry's bytes stay local to where its job runs. Runners outside the service, such as a hosted provider's machines, never download from or upload to the service's bucket, since internet egress is too costly per byte: their bytes live in the runner's own store, such as its Actions cache, and they fetch sources from upstream on a miss.

- **The service's store,** `service`, holds bytes, in the cache's bucket, and is open only to jobs on the service's own machines: a request names it only from a job whose `check_run_id` GitHub shows running on one of them, matched by the runner's name, and that its machine's job-started check admitted (specs/0002-machines.md). Its restores and saves hand out signed URLs, as below.
- **An index** is any other store, named by the consumer, 1 to 16 lowercase letters and digits, such as `actions`. Its entries hold no bytes: a save records the size and SHA-256 of what the job keeps in its own store, then a commit, and a restore answers the entry's scope, key, step, size and SHA-256, with no URL. The job checks the bytes it finds in its own store against them, and treats a mismatch or a miss as nothing restored. No bucket object, upload or download is involved.

Stores are kept apart: a request is offered only entries of the store it names. Every entry, whatever its store, keeps every other rule here: the scopes, the restore order and the run's choice, success gating, one entry of each family in a pull request, and retention.

**What the index is for.** A runner's own store has scopes of its provider's, not the service's: an approved contribution, for one, may be able to write to the default branch's scope of the runner's Actions cache. The index is what keeps untrusted code from poisoning a later main or published build there: a job of main's restores only bytes whose SHA-256 an entry of main's restore chain names, and only a successful job of a run bound to that scope commits one.

## Why a cache, and not something else

What a job outside the service needs restored to start warm is build state, not downloads. A proxy, the service's earlier design, caches only what it fetches from upstream, and its git mirror served only the service's own machines. A general one, such as SOCKS, tunnels bytes and caches nothing; caching HTTPS through it would mean intercepting TLS with a certificate authority on every machine; and git's fetch is a `POST`, which HTTP caches don't keep. GitHub's own Actions cache has scopes that aren't the service's: an untrusted trigger can restore from the default branch's scope but saves nothing of its own, and the guarantee that nothing of a pull request reaches main would rest on GitHub's rules rather than the service's run binding. Its quotas aren't the service's either. So the cache's scope is the run's binding, as a saved disk's is.

## Who may use it

Every request carries the job's GitHub OIDC token, with audience `endpoints.Service`, which the service verifies as it does for the run binding: signed by GitHub's published keys, unexpired, and from a repository whose owner ID is the organization's (specs/0001-architecture.md, Endpoints). Then:

- **The job must be running.** The token's `check_run_id` names its job, and GitHub must show that job in progress, in the token's repository, run and run attempt. A token without `check_run_id` is refused. A token is good only while its own job runs, whoever holds it.
- **The run must be bound.** The cache never makes a binding: the run's `runners` job binds it first (specs/0005-interface.md), and every request uses the scope the binding holds. A request from a run with no binding is refused.
- **Forks get nothing.** A run whose binding names a head repository other than the consumer is refused, unless it's an approved contribution: a `pull_request_target` run for which GitHub records an approval of the `contribution` environment, as the job-started check requires (specs/0001-architecture.md, Forks and contributions).
- **The scope decides the rest.** A restore reads the run's restore chain. A save writes only the run's own scope; a `read` run restores from main's, and saves nothing.

**What a stolen credential gives.** A job's own code can take its token, or a signed URL, or an upload's session URI, and hand it to anything. None of them lets its holder do more than the job itself may: read the entries of the run's restore chain, and write entries in the run's own scope, and then only until the job ends, for the token, or until the URL expires. An upload's session URI outlives both, for up to a week, as Cloud Storage keeps a resumable upload open; but it can only write the one upload object its reservation named, which nothing restores until the same job commits it before the reservation expires, and which the service deletes once the reservation has expired. An entry written that way becomes restorable only if the job succeeds (below), and only in the scope the job could have written anyway.

**Approved contributions** need `id-token: write` in the jobs that use the cache, normally those on runners outside the service, which also lets the contributor's code mint tokens of the base repository, for any audience, while its job runs. Such a token can do no more with the service than its job may:

- **It can't change its run's scope.** A run is bound once; `/v1/bind` answers every later call with the binding the run has, which in a contribution's run its bootstrap workflow made, before any job that runs the contributor's code could start, since those jobs need its output. A `pull_request_target` run's pull request scope is bound only from the bootstrap's own `job_workflow_ref`, which a contribution's job never has (specs/0002-machines.md, Scope).
- **It can't act on another run.** Every request acts on the token's own repository, run, attempt and job, as GitHub signed them; no body names a run, a scope or a job.
- **It gets nothing else.** The cache answers it only with its run's restore chain and its own scope's saves; the job-started check and report take only a machine's identity, never a GitHub token.

Every party that trusts the consumer's tokens, other than this service, must refuse one whose `event_name` is `pull_request_target`, as the consumer's own workload identity federation already must (specs/0001-architecture.md, Forks and contributions; specs/0005-interface.md, The cache). A consumer that can't promise that doesn't use the cache in its contribution jobs.

Never log a token, a signed URL or an upload's session URI.

## Entries

An entry belongs to a consumer, a scope, a platform, a family and a key:

- **Family and key** follow the selectors' rules (specs/0005-interface.md): a family is 1 to 32 lowercase letters, digits and hyphens, starting with a letter; a key is 20 lowercase hex digits, which the consumer computes from whatever the entry's contents depend on.
- **Platform** is `<os>-<arch>`, the runner's operating system and architecture, each 1 to 16 lowercase letters and digits, such as `linux-x64`, `linux-arm64` or `macos-arm64`. Entries are kept apart by platform, as disks are by architecture: one saved on one platform is never restored on another. The platform is the job's word, and it decides only which of the entries the job may read anyway it's given.
- **Store** is the store the entry is in (Stores, above).
- **Series** is the first 20 hex digits of the SHA-256 of the scope, store, platform, family and key, joined by NUL characters. Saving again for the same series makes a new version.

The service keeps its records in its own bucket, and the bytes in the cache's:

```
lanescape-runners:
  cache/reservations/<repository ID>/<reservation ID>.json  an upload, until it's committed or expires
  cache/entries/<repository ID>/<entry ID>.json             an entry of any store: pending, ready or marked
  cache/choices/<repository ID>/<run ID>/<hash>.json         a run's restore choice for a store, family, platform and key
  cache/recent/<repository ID>/<scope>/<hash>.json           a pull request's most recent choice for a store, family and platform
  cache/families/<repository ID>/<hash>.json                 when a store's family and platform of main's or a branch's was last used
  closures/<repository ID>/<scope>.json                      the last time the service saw a pull request closed, shared with the disks
  leases/disks/<repository ID>                               the consumer's short lease for choosing and marking, the disks' too
lanescape-runners-cache:
  uploads/<repository ID>/<reservation ID>                   an upload's object
  entries/<repository ID>/<series>/<entry ID>                an entry's object, in the service's store
```

Every record is written before what it describes, as the disks' are (specs/0003-disks.md), and an object of the cache's names with no record goes, under the lease, as an orphaned image does.

## Restoring

A restore asks for a store, family, platform and key. The service picks once per run and entry, the first time a job of the run asks, and records the choice; every later request of the run for the same store, family, platform and key gets the same entry, even after a newer one is committed. It picks the first that exists, of the store's entries, of:

1. the first scope of the run's restore chain's ready entry for that family, platform and key (specs/0002-machines.md, Scope);
2. that scope's newest ready entry of that family and platform, whatever its key;
3. if the chain has a second scope, its base, the same two in it;
4. none.

For a pull request, that's its own entry for the key, its own newest, then its base's two, the listed branch's it targets or else main's; for main and `read` runs, main's two; for a long-lived branch's push, its own two, then main's.

Of several versions of a series, the newest comes first; a marked entry is never chosen. The newest of a family is how a job whose key has changed still starts from the state nearest its own; a consumer's job must check what it restored and rebuild what's out of date.

**Why a choice holds for the run.** Without it, a run's later job could restore a sibling's newly saved entry or not, depending on how soon the service saw the sibling succeed; with it, every job of the run with the same family, platform and key starts from the same state, whatever order they run in. The choice is also what keeps the entry from being deleted while the run may still need it, as a disk's restore choice keeps its image (specs/0003-disks.md). It holds as a disk's does: for the attempts that the service's own automatic reruns start, by GitHub's record of who triggered each; any other attempt picks again, as a new run would. A run's choices are dropped once it has finished and has no automatic rerun left.

**A lookup** asks for the same choice without the URL: the service picks and records the run's choice as a restore would, so a later restore of the run gets the same entry, but hands out no URL and doesn't mark the entry used. It's for a job that wants to know what it would restore, and whether its key is exact, before deciding to.

**What each restore found** is recorded, so the order and what's kept can be tuned from evidence: the choice's record names the step of the order it came from, `pull_request_exact`, `pull_request_newest`, `main_exact`, `main_newest`, `branch_exact`, `branch_newest` or `none`, with its family, platform and scope, and the service writes the same in a structured log line. Neither holds a URL, a token or anything of the entry's contents.

The answer names the chosen entry with a V4 signed URL for a `GET` of its object, at the generation its record names, valid for 15 minutes. The job reads it in ranges, and asks again for a new URL whenever it needs one: the same request answers the same choice. The answer also carries the size and SHA-256 recorded at commit, which the job checks over what it read before using any of it; a mismatch counts as nothing restored. A choice's use marks the entry used, at most once a day, in the same write under the lease that the choice makes.

## Saving

A save in an index is two steps: the job records the entry's size and SHA-256 with a reservation, and commits it, saying them again, before the reservation expires. The service writes the entry's record as `pending`, with when its run was bound, and nothing is uploaded or copied. A save in the service's store, from one of its machines, is three steps:

1. **Reserve.** The service records a reservation for the job, by its `check_run_id`, with the run's scope and the entry's platform, family and key, and hands out a V4 signed URL that starts a resumable upload of one object, `uploads/<repository ID>/<reservation ID>`. Its signed headers include `x-goog-resumable: start` and `x-goog-if-generation-match: 0`, so it can only create that object, once. It's valid for an hour, which is the reservation's lifetime.
2. **Upload.** The job starts the upload with the signed URL, and sends the bytes in chunks to the session URI Cloud Storage answers with, resuming after a failure as Cloud Storage's resumable uploads allow. The session URI is as much a credential as the signed URL.
3. **Commit.** Before the reservation expires, the same job sends the reservation's ID, the object's size and its SHA-256. The service checks that the upload's object exists and has exactly that size, writes the entry's record as `pending`, with when its run was bound, copies the object within the bucket to `entries/<repository ID>/<series>/<entry ID>`, only if absent, records the copy's generation, and deletes the upload. A copy within one bucket, location and storage class is Cloud Storage's to make; the bytes don't pass through the service. The size and SHA-256 are the job's word, which every restore checks: a false one only makes later restores of the job's own scope count as missing.

An upload never committed is never restorable. Once a reservation has expired, the service refuses its commit and deletes whatever is at its upload's name, including an upload that finishes later through its session URI; the bucket's lifecycle rule deletes anything under `uploads/` a day old, as the backstop for one the service missed.

**Only a successful job's entries are kept,** and the service enforces it. A committed entry stays `pending` until GitHub shows its job finished: with conclusion `success` it becomes `ready`, and restorable, and with anything else it's deleted. The service learns of it from GitHub's `workflow_job` delivery, and each pass checks every pending entry, in case a delivery was lost. GitHub ends every job within 5 days, so a pending entry whose job isn't shown finished by then is deleted. The service rather than the action decides, because the token's `check_run_id` ties every commit to one job, and GitHub's conclusion for that job is exactly what saved disks are gated on; the action also saves only once a job's earlier steps have succeeded, which saves a doomed upload, but a job's own code can call the interface itself. Success is a quality rule, keeping a failed job's half-written state from being restored; what keeps scopes apart is the binding. So an entry becomes restorable only once its job has ended, and a later job of its own run that asks for the same family, platform and key gets the run's choice, made before.

## Deleting

Nothing has a size limit. Every entry goes as early as nothing needs it, under the consumer's short lease, the one its disks' steps take, `leases/disks/<repository ID>`, apart from the pass's, held only for one step, which waits for it and finishes within 30 seconds: picking and recording a choice, reading a recorded one, making an entry ready, dropping a finished run's choices, and marking an entry for deletion each happen under it. It's the disks' lease, rather than one of the cache's own, because a closure is shared, and written only under that lease (specs/0003-disks.md, Expiry): so no step of the cache's reads one while it changes, and no lease is ever taken inside another. An entry is deleted only after it's marked; a marked entry is never chosen, and no URL is signed for it. Marking writes the entry's record only at the generation it was read at when the decision was made, before the choices were read, and a choice, or each reading of one, rewrites the record, so a mark can't succeed past a choice. No rule deletes an entry that an unfinished run's recorded choice names; it goes the moment that stops being true. Once its record is marked, its object is deleted, then its record.

- **Superseded versions.** Once a newer version of a series is ready, the older ones go.
- **A pull request keeps one entry of each family and platform.** When one of its entries becomes ready, its other entries of that family and platform go, whatever their key. The restore order is unchanged. A family is one lineage: jobs that need several keys alive at once use separate families, or each replaces the others.
- **A closed pull request's scope goes at once,** merged or not. The service acts on GitHub's `pull_request` delivery with action `closed`, and each pass reads the pull request of every scope it holds, in case a delivery was lost. An entry committed by a run bound up to the last time the service saw its pull request closed, from the delivery or a pass, is never chosen again and replaces nothing, as a disk doesn't (specs/0003-disks.md, Expiry), so a reopened pull request starts again from its base scope. Whether an entry that replaces another is retired is read again under the lease before the other is marked, since a closing may be recorded meanwhile. An open pull request's entries go 14 days after their last use, for one abandoned and never closed.
- **Main keeps the two newest keys of each family and platform,** the newest and the one before it, by their newest versions, and any older key that an open pull request's most recent choice names. A pull request's most recent choice for a family and platform is recorded apart from its run's, in `cache/recent/`, and kept until the pull request closes. Whether a key is kept is checked again whenever main commits, a pull request chooses or a pull request closes. The previous key is kept for a pull request branched before main's dependencies moved that hasn't run since, and so has no choice recorded yet, and for main reverting a move: either would otherwise rebuild from nothing. A family and platform of main's that nobody has used for 90 days goes, newest included: it's been retired. Its last use is recorded apart from its versions, and read again under the lease before any of it is marked, as a disk family's is, so deleting a version never makes it look unused for longer than it has been.
- **A long-lived branch's scope** keeps what main's does, for its own pushes and the pull requests into it, and goes at once when its branch is no longer listed in the default branch's `.github/runners.json`, or no longer exists.
- **Uncommitted uploads and unsuccessful entries** go as Saving says.

**Considered, and not done: a pull request's entry as a delta over the main entry it restored.** It would store less for each pull request, but it would bind every such entry to its main base, which then couldn't go early, and it adds a step to every restore. It could be revisited if pull requests' entries, measured, turn out large.

## Storage and billing

`lanescape-runners-cache` is a bucket in the service's project, in `northamerica-northeast1`, which `infra/` makes, with public access prevention and uniform access, and the service's account as the only identity granted on it. It's Standard class, since the other classes charge for deleting an object early, and entries go as early as their rules allow. It keeps no versions, and soft delete is off, which would otherwise bill every deleted entry for 7 more days. The service signs URLs with IAM's `signBlob` as its own account, which holds no key, under a grant on its own account and nothing else (specs/0004-deployment.md, Bootstrap); a signed URL acts as the service, for one object and one method, until it expires.

Storage is billed per GB-month. Uploads cost nothing per byte; each request is an operation, and a commit's copy is one more, while its bytes are stored twice until the upload is deleted. Only the service's own machines, in the same region, read and write the bucket, so no restore pays internet egress: runners outside Google Cloud, which would pay it per byte, keep their bytes in their own stores, and the service holds only their index (Stores, above).
