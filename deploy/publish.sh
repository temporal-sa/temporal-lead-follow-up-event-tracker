#!/usr/bin/env bash
set -euo pipefail

tracker_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
gitops_dir="${GITOPS_DIR:-$HOME/Source/cluster-gitops-config}"
repository="temporal-sa/temporal-lead-follow-up-event-tracker"
repository_url="https://github.com/$repository"

# Run outside the restricted agent session. Preserve configured commit signing.
gh api user --jq .login
if [[ "$(git -C "$gitops_dir" branch --show-current)" != main ]]; then
  echo "The GitOps checkout must be on main." >&2
  exit 1
fi
if [[ -n "$(git -C "$gitops_dir" status --porcelain)" ]]; then
  echo "The GitOps checkout must be clean before publishing." >&2
  exit 1
fi
git -C "$gitops_dir" pull --ff-only origin main

if [[ ! -e "$tracker_dir/.git" ]]; then
  git -C "$tracker_dir" init -b main
fi
if [[ "$(git -C "$tracker_dir" rev-parse --show-toplevel)" != "$tracker_dir" || "$(git -C "$tracker_dir" branch --show-current)" != main ]]; then
  echo "The tracker checkout must be its own repository on main." >&2
  exit 1
fi
git -C "$tracker_dir" add .
if ! git -C "$tracker_dir" diff --cached --quiet; then
  git -C "$tracker_dir" commit -m "Add Temporal event lead tracker"
fi

if ! gh repo view "$repository" --json url >/dev/null 2>&1; then
  gh repo create "$repository" --public --description "Mobile event lead forms, employee controls, QR codes, and sharded Temporal workflows"
fi
if [[ "$(gh repo view "$repository" --json isPrivate --jq .isPrivate)" != false ]]; then
  echo "The target repository is private; inspect it before publishing." >&2
  exit 1
fi
if git -C "$tracker_dir" remote get-url origin >/dev/null 2>&1; then
  case "$(git -C "$tracker_dir" remote get-url origin)" in
    "$repository_url"|"$repository_url.git"|"git@github.com:$repository.git") ;;
    *) echo "The tracker origin points at a different repository." >&2; exit 1 ;;
  esac
else
  git -C "$tracker_dir" remote add origin "$repository_url.git"
fi
git -C "$tracker_dir" push -u origin main

cp "$tracker_dir/deploy/demo-project.yaml" "$gitops_dir/projects/demo/event-leads.yaml"
python=python3
if [[ -x "$gitops_dir/registry/worker/.venv/bin/python" ]]; then
  python="$gitops_dir/registry/worker/.venv/bin/python"
fi
(
  cd "$gitops_dir"
  PYTHONDONTWRITEBYTECODE=1 "$python" scripts/validate_projects.py
  git add projects/demo/event-leads.yaml
  git diff --cached --check
  if ! git diff --cached --quiet; then
    git commit -m "Deploy event lead tracker demo"
  fi
  git push origin main
)

echo "Published $repository_url and the GitOps manifest."
echo "Once reconciliation finishes, open https://event-leads.tmprl-demo.cloud/admin"
