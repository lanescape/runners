#!/usr/bin/env bash
# Refuses Terraform that infra/, bootstrap/ and tools/ must never hold
# (specs/0004-deployment.md, specs/0002-machines.md, specs/0006-tools.md).
# Run from the repository's root; ci.yml runs it on every pull request.
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
refuse "infra/, bootstrap/ and tools/ may not use modules" '^[[:space:]]*module[[:space:]]+"' infra bootstrap tools
if found=$(find infra bootstrap tools -name '*.tf.json' -o -name '*.tfvars.json' -o -name '*.tftest.json' | grep .); then
  echo "infra/, bootstrap/ and tools/ may not hold JSON configuration, which these checks don't read:"
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
  'resource[[:space:]]+"google_[a-z0-9_]*_iam_(policy|binding)"' infra bootstrap tools

# The tools' bucket holds files that may never be published or
# redistributed (specs/0006-tools.md): no grant to everyone, signed in or
# not, no ACLs, and public access prevention stays enforced.
refuse "tools/ may grant nothing to the public" 'allUsers|allAuthenticatedUsers' tools
refuse "tools/ may not use ACLs" 'resource[[:space:]]+"google_storage_[a-z_]*(access_control|acl)"' tools

# Each bucket's own settings, at the top level of its resource block, so
# that a setting elsewhere, in another block, a local or a comment, can't
# stand in. Each line is read a character at a time, as HCL reads it:
# comments are dropped, and braces count only outside strings, however
# their interpolations nest. A block left open at the end of its file
# counts as unsafe. Heredocs, whose lines this
# can't tell from code, are refused.
refuse "tools/ may not use heredocs, which the bucket check can't read" '<<-?[[:space:]]*[A-Za-z_]' tools
if found=$(awk '
  FNR == 1 {
    if (inb) print where
    inb = 0; incomment = 0; sp = 0
  }
  {
    # stack[1..sp] holds what encloses the text: "s", a string; "i", an
    # interpolation in one, ${ or %{; "b", a brace inside an interpolation.
    code = ""; opens = 0; closes = 0
    n = length($0); i = 1
    while (i <= n) {
      c = substr($0, i, 1); c2 = substr($0, i, 2)
      if (incomment) {
        if (c2 == "*/") { incomment = 0; code = code " "; i += 2 } else i++
        continue
      }
      if (sp && stack[sp] == "s") {
        if (c == "\\" || c2 == "$$" || c2 == "%%") { code = code c2; i += 2; continue }
        if (c2 == "${" || c2 == "%{") { stack[++sp] = "i"; code = code c2; i += 2; continue }
        if (c == "\"") sp--
        code = code c; i++
        continue
      }
      if (sp) {
        if (c == "\"") stack[++sp] = "s"
        else if (c == "{") stack[++sp] = "b"
        else if (c == "}") sp--
        code = code c; i++
        continue
      }
      if (c == "#" || c2 == "//") break
      if (c2 == "/*") { incomment = 1; i += 2; continue }
      if (c == "\"") stack[++sp] = "s"
      if (c == "{") opens++
      if (c == "}") closes++
      code = code c; i++
    }
  }
  !inb && code ~ /^[[:space:]]*resource[[:space:]]+"google_storage_bucket"[[:space:]]/ {
    inb = 1; depth = 0; pap = 0; ubla = 0; where = FILENAME ":" FNR
  }
  inb {
    if (depth == 1 && code ~ /^[[:space:]]*public_access_prevention[[:space:]]*=[[:space:]]*"enforced"[[:space:]]*$/) pap = 1
    if (depth == 1 && code ~ /^[[:space:]]*uniform_bucket_level_access[[:space:]]*=[[:space:]]*true[[:space:]]*$/) ubla = 1
    depth += opens - closes
    if (opens + closes > 0 && depth <= 0) {
      if (!pap || !ubla) print where
      inb = 0
    }
  }
  END {
    if (inb) print where
  }
' tools/*.tf | grep .); then
  echo "Every bucket in tools/ must set public_access_prevention = \"enforced\" and uniform_bucket_level_access = true:"
  echo "$found"
  failed=1
fi

if [ "$failed" != 0 ]; then
  exit 1
fi
echo "infra/, bootstrap/ and tools/ hold nothing they may not."
