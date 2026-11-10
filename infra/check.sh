#!/usr/bin/env bash
# Refuses Terraform that infra/ and bootstrap/ must never hold
# (specs/0004-deployment.md, specs/0002-machines.md). Run from the
# repository's root; ci.yml runs it on every pull request.
set -euo pipefail

cd "$(dirname "$0")/.."
failed=0

refuse() {
  local why=$1 pattern=$2
  shift 2
  local found
  if found=$(grep -rEn --include='*.tf' -e "$pattern" "$@"); then
    echo "$why:"
    echo "$found"
    failed=1
  fi
}

# Infrastructure Manager runs no provisioners or external programs as the
# infra identity, and nothing from outside this directory: no modules, and
# no JSON configuration a reader, or these checks, could miss.
refuse "infra/ may not use provisioners" '(^|[^a-z_])provisioner([^a-z_]|$)|(^|[^a-z_])connection[[:space:]]*\{' infra
refuse "infra/ may not run programs" 'local-exec|remote-exec' infra
refuse "infra/ may not use external data sources or the external provider" '"external"|hashicorp/external' infra
refuse "infra/ and bootstrap/ may not use modules" '^[[:space:]]*module[[:space:]]+"' infra bootstrap
if found=$(find infra bootstrap -name '*.tf.json' -o -name '*.tfvars.json' -o -name '*.tftest.json' | grep .); then
  echo "infra/ and bootstrap/ may not hold JSON configuration, which these checks don't read:"
  echo "$found"
  failed=1
fi

# The network and its subnetwork are bootstrap/'s; the infra identity can't
# change them, and infra/ may not try. The network has no firewall rules at
# all, so nothing reaches a machine: neither may declare one, nor a policy.
refuse "infra/ may not manage networks or firewall rules" \
  'resource[[:space:]]+"google_compute_(network|subnetwork|firewall|[a-z_]*firewall_policy[a-z_]*|route|router[a-z_]*)"' infra
refuse "The network has no firewall rules or policies" \
  'resource[[:space:]]+"google_compute_(firewall|[a-z_]*firewall_policy[a-z_]*)"' bootstrap

# Accounts, project-level grants, role definitions and log sinks are
# bootstrap/'s, so a merged change can't widen any account's access.
refuse "infra/ may not manage accounts, project-level grants, roles or log sinks" \
  'resource[[:space:]]+"google_(service_account[a-z_]*|project_iam[a-z_]*|organization_[a-z_]*|folder_[a-z_]*|logging_[a-z_]*sink[a-z_]*|iam_[a-z_]*|project_service)"' infra

# The project is shared: every grant adds a member, and none replaces a
# policy or a role's members.
refuse "Grants must add members, never replace a policy or a role's members" \
  'resource[[:space:]]+"google_[a-z0-9_]*_iam_(policy|binding)"' infra bootstrap

if [ "$failed" != 0 ]; then
  exit 1
fi
echo "infra/ and bootstrap/ hold nothing they may not."
