# Bootstrap

What `infra/` can't make for itself, or mustn't be able to change (specs/0004-deployment.md, Bootstrap). A maintainer applies it by hand, with their own credentials, and again whenever it changes. Nothing applies it automatically.

It holds:

- the APIs, which stay enabled if they're removed from here, since other teams use them too;
- the Terraform state bucket, `lanescape-runners-tfstate`, which holds this configuration's state, and `lanescape-runners-infra-manager`, where Infrastructure Manager keeps the deployment's artifacts and logs (it keeps `infra/`'s state itself);
- the Artifact Registry repository, `lanescape-runners`;
- the Cloud Run service, `lanescape-runners`, as an unreachable shell that `infra/` adopts and deploys, with the infra identity's grant on it alone;
- every service account: the service's, the machines' (with no role anywhere), the scheduler's, the push subscriptions', CD's (`lanescape-runners-deployer`) and the infra identity (`lanescape-runners-infra`);
- the service's custom roles and every project-level grant, each limited by IAM Conditions to the service's own resources by name wherever IAM can do that;
- the network, `lanescape-runners`, its subnetwork, with no firewall rules, and who may attach to the subnetwork;
- the Pub/Sub topics and push subscriptions, the scheduler job, and the log sink for Compute Engine's preemption and guest-shutdown events of the service's machines, with its fixed filter;
- for the machines' boot logs (specs/0002-machines.md), a log bucket of the service's own, `lanescape-runners-serial`, a sink with a fixed filter that routes there only the serial console entries of the service's machines, and a view of it that only the service's account may read;
- CD's workload identity pool, `lanescape-runners-github`, whose provider accepts only a push to `main` by `cd.yml` in this repository, checked by the repository's and the organization's IDs.

The network, Pub/Sub, the scheduler job and the Cloud Run service's shell are here rather than in `infra/`, as specs/0004-deployment.md lists them, because IAM can't limit any of them by name: an identity that could make them could change every other team's networks, topics, subscriptions, jobs and services in the project too. The first three don't change with a deploy. A shell runs Google's sample container as the machines' account, with internal ingress and Cloud Run's invoker check, and once `infra/` has adopted it, this configuration ignores it.

## What must exist first

- **Your access:** Owner of `lanescape-dev`, or enough to enable APIs, make service accounts, custom roles, project-level grants and log sinks.
- **The "Lanescape Runners" GitHub App,** private to the organization (installable only on its owner's account), installed on it, with the permissions specs/0001-architecture.md describes, Pull requests: read among them, and its webhook at `<service_url>/v1/webhook` delivering `workflow_job`, `workflow_run` and `pull_request` (specs/0004-deployment.md, The GitHub App). The service's URL is Cloud Run's deterministic one, `https://lanescape-runners-<project number>.northamerica-northeast1.run.app`, which `terraform output service_url` also gives. Keep its private key and webhook secret for after the first deploy (`infra/README.md`).
- **The state bucket,** made by hand once, since this configuration's state lives in it:

  ```sh
  gcloud storage buckets create gs://lanescape-runners-tfstate --project=lanescape-dev \
    --location=northamerica-northeast1 --uniform-bucket-level-access --public-access-prevention
  gcloud storage buckets update gs://lanescape-runners-tfstate --versioning
  ```

  The first apply adopts it (`state.tf`).

## Applying

```sh
cd bootstrap
gcloud auth application-default login
terraform init
terraform plan -out=bootstrap.tfplan
terraform apply bootstrap.tfplan
```

Read the plan before applying it: it's the only review this configuration's changes get beyond the pull request's. `infra/check.sh`, which CI runs, refuses any firewall rule or policy, and any grant that replaces a policy or a role's members rather than adding one.

## After the first apply

1. Give CD what it needs from `terraform output`: the workload identity provider and the deployer's account for signing in, the image repository for `ko`, and the Infrastructure Manager artifacts bucket. `infra/README.md` says how CD starts an apply.
2. Make sure the organization requires approval to run fork pull request workflows from every outside collaborator, which the service checks at deploy (specs/0002-machines.md).
3. Let CD deploy `infra/` for the first time, then add the App's secrets as `infra/README.md` says.

## What IAM can't limit

IAM Conditions limit grants by name only for some resource types. The grants here that can't be limited, and so reach other teams' resources in the shared project, are:

- **Infrastructure Manager:** CD may create, read and update any deployment in the project, and start previews; the infra identity holds the agent's state permissions on any deployment, which include reading and replacing another deployment's Terraform state. CD can't delete a deployment, and can't make one run as any account but the infra identity. IAM can't limit deployments by name, and the provider can't grant on a deployment itself.
- **Making things:** the infra identity may create buckets and secrets of any name, which touches nothing that exists, but can manage only its own, by name.
- **Lists, operations and reads:** the service may list every instance, disk and image in the project, and read zones, machine types and operations; the infra identity may read the project and Cloud Run's operations, which Terraform polls and IAM can't grant on one at a time. It can't list operations or read logs, so it has no way to learn another team's operation's name.

Only a project of the service's own would remove these; for Infrastructure Manager, a project of its own that holds only this deployment would.
