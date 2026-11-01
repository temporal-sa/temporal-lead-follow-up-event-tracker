#!/usr/bin/env bash
# Creates the Event Lead Tracker's permanent storage in the demo AWS account: the
# DynamoDB table and the IAM roles the web and worker components assume through
# IRSA. It lives outside the catalog and the platform's infrastructure on purpose:
# finalizing or recreating the catalog entry, or replacing its Temporal namespace,
# must not touch it.
#
# Safe to run again: it creates what's missing and resets the roles' trust and
# permissions to what's below. Run it again when a platform cluster is added or
# replaced, so the roles trust the new cluster's OIDC provider.
set -euo pipefail

region="${AWS_REGION:-us-west-1}"
prefix=tmprl-dem-cld
table="$prefix-event-leads"
namespace="$prefix-event-leads"
export AWS_REGION="$region" AWS_PAGER=""

account="$(aws sts get-caller-identity --query Account --output text)"
if [[ "$account" != 429214323166 ]]; then
  echo "Expected the demo account 429214323166, got $account." >&2
  exit 1
fi

# The table. The schema matches internal/store.
if aws dynamodb describe-table --table-name "$table" >/dev/null 2>&1; then
  echo "Table $table exists."
else
  index() { echo "{\"IndexName\":\"$1\",\"KeySchema\":[{\"AttributeName\":\"$2\",\"KeyType\":\"HASH\"},{\"AttributeName\":\"$3\",\"KeyType\":\"RANGE\"}],\"Projection\":{\"ProjectionType\":\"ALL\"}}"; }
  aws dynamodb create-table --table-name "$table" --billing-mode PAY_PER_REQUEST \
    --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S \
      AttributeName=gsi1pk,AttributeType=S AttributeName=gsi1sk,AttributeType=S \
      AttributeName=gsi2pk,AttributeType=S AttributeName=gsi2sk,AttributeType=S \
      AttributeName=lsi1sk,AttributeType=S \
    --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \
    --global-secondary-indexes "[$(index gsi1 gsi1pk gsi1sk),$(index gsi2 gsi2pk gsi2sk)]" \
    --local-secondary-indexes "[$(index lsi1 pk lsi1sk)]" \
    --deletion-protection-enabled \
    --tags Key=project,Value=event-leads Key=managed-by,Value=temporal-lead-follow-up-event-tracker/deploy/create-store.sh >/dev/null
  aws dynamodb wait table-exists --table-name "$table"
  echo "Created table $table."
fi
aws dynamodb update-continuous-backups --table-name "$table" \
  --point-in-time-recovery-specification PointInTimeRecoveryEnabled=true >/dev/null
table_arn="$(aws dynamodb describe-table --table-name "$table" --query Table.TableArn --output text)"

# Every platform cluster's OIDC issuer, so the roles work on whichever one runs the project.
issuers=()
for cluster in $(aws eks list-clusters --query "clusters[?starts_with(@, '$prefix-')]" --output text); do
  issuers+=("$(aws eks describe-cluster --name "$cluster" --query cluster.identity.oidc.issuer --output text | sed 's|^https://||')")
done
if (( ${#issuers[@]} == 0 )); then
  echo "No $prefix-* EKS clusters found in $region." >&2
  exit 1
fi

# Workflows are the only writers, and they run on the worker. The web component
# can only read, which enforces that boundary.
declare -A actions=(
  [worker]='"dynamodb:DescribeTable","dynamodb:GetItem","dynamodb:Query","dynamodb:PutItem","dynamodb:UpdateItem","dynamodb:BatchWriteItem","dynamodb:ConditionCheckItem"'
  [web]='"dynamodb:DescribeTable","dynamodb:GetItem","dynamodb:BatchGetItem","dynamodb:Query"'
)
for component in worker web; do
  role="$prefix-event-leads-$component"
  statements=()
  for issuer in "${issuers[@]}"; do
    # A rollout runs in "<project namespace>-<5 hex characters>". Matching exactly
    # five characters keeps a future "event-leads-…" project from assuming the role.
    statements+=("{\"Effect\":\"Allow\",\"Principal\":{\"Federated\":\"arn:aws:iam::$account:oidc-provider/$issuer\"},\"Action\":\"sts:AssumeRoleWithWebIdentity\",\"Condition\":{\"StringEquals\":{\"$issuer:aud\":\"sts.amazonaws.com\"},\"StringLike\":{\"$issuer:sub\":[\"system:serviceaccount:$namespace:$component\",\"system:serviceaccount:$namespace-?????:$component\"]}}}")
  done
  trust="{\"Version\":\"2012-10-17\",\"Statement\":[$(IFS=,; echo "${statements[*]}")]}"
  if aws iam get-role --role-name "$role" >/dev/null 2>&1; then
    aws iam update-assume-role-policy --role-name "$role" --policy-document "$trust"
    echo "Updated role $role."
  else
    aws iam create-role --role-name "$role" --assume-role-policy-document "$trust" \
      --description "Event Lead Tracker $component access to DynamoDB $table" \
      --tags Key=project,Value=event-leads Key=managed-by,Value=temporal-lead-follow-up-event-tracker/deploy/create-store.sh >/dev/null
    echo "Created role $role."
  fi
  aws iam put-role-policy --role-name "$role" --policy-name "event-leads-$component" \
    --policy-document "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":[${actions[$component]}],\"Resource\":[\"$table_arn\",\"$table_arn/index/*\"]}]}"
done

echo "Table: $table_arn"
echo "Roles: arn:aws:iam::$account:role/$prefix-event-leads-worker, arn:aws:iam::$account:role/$prefix-event-leads-web"
echo "Trusted OIDC issuers: ${issuers[*]}"
