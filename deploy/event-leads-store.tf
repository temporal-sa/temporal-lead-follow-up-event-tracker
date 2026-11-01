# Permanent storage for the Event Lead Tracker. Copy this file into the platform
# repository's infra/ directory and apply it with the rest of the platform's
# OpenTofu. It lives outside the DemoProject on purpose: finalizing or recreating
# the catalog entry, or replacing its Temporal namespace, must not touch it.
#
# It follows infra/advanced_ai_agent.tf and uses that file's var.name_prefix,
# var.colors, and local.account_id. The schema matches internal/store.

data "aws_eks_cluster" "event_leads" {
  for_each = toset(var.colors)
  name     = "${var.name_prefix}-${each.key}"
}

locals {
  event_leads_namespace = "${var.name_prefix}-event-leads"
  # A rollout runs in "<project namespace>-<5 hex characters>". Matching exactly
  # five characters keeps a future "event-leads-…" project from assuming the roles.
  event_leads_subjects = {
    for component in ["web", "worker"] : component => [
      "system:serviceaccount:${local.event_leads_namespace}:${component}",
      "system:serviceaccount:${local.event_leads_namespace}-?????:${component}",
    ]
  }
}

resource "aws_dynamodb_table" "event_leads" {
  name         = "${var.name_prefix}-event-leads"
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "pk"
  range_key    = "sk"

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }
  attribute {
    name = "gsi1pk"
    type = "S"
  }
  attribute {
    name = "gsi1sk"
    type = "S"
  }
  attribute {
    name = "gsi2pk"
    type = "S"
  }
  attribute {
    name = "gsi2sk"
    type = "S"
  }
  attribute {
    name = "lsi1sk"
    type = "S"
  }

  # The employee event directory, newest last.
  global_secondary_index {
    name            = "gsi1"
    hash_key        = "gsi1pk"
    range_key       = "gsi1sk"
    projection_type = "ALL"
  }
  # Events that still need a running workflow; the reconciler restarts them.
  global_secondary_index {
    name            = "gsi2"
    hash_key        = "gsi2pk"
    range_key       = "gsi2sk"
    projection_type = "ALL"
  }
  # Participants in first-submitted order. A local index can only be created with the table.
  local_secondary_index {
    name            = "lsi1"
    range_key       = "lsi1sk"
    projection_type = "ALL"
  }

  point_in_time_recovery { enabled = true }
  deletion_protection_enabled = true
  lifecycle { prevent_destroy = true }
}

resource "aws_iam_role" "event_leads" {
  for_each = local.event_leads_subjects
  name     = "${var.name_prefix}-event-leads-${each.key}"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [for color, cluster in data.aws_eks_cluster.event_leads : {
      Effect    = "Allow"
      Principal = { Federated = "arn:aws:iam::${local.account_id}:oidc-provider/${trimprefix(cluster.identity[0].oidc[0].issuer, "https://")}" }
      Action    = "sts:AssumeRoleWithWebIdentity"
      Condition = {
        StringEquals = { "${trimprefix(cluster.identity[0].oidc[0].issuer, "https://")}:aud" = "sts.amazonaws.com" }
        StringLike   = { "${trimprefix(cluster.identity[0].oidc[0].issuer, "https://")}:sub" = each.value }
      }
    }]
  })
}

# Workflows are the only writers, and they run on the worker. The web component
# can only read, which enforces that boundary.
resource "aws_iam_role_policy" "event_leads" {
  for_each = {
    worker = ["dynamodb:DescribeTable", "dynamodb:GetItem", "dynamodb:Query", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:BatchWriteItem", "dynamodb:ConditionCheckItem"]
    web    = ["dynamodb:DescribeTable", "dynamodb:GetItem", "dynamodb:BatchGetItem", "dynamodb:Query"]
  }
  name = "event-leads-${each.key}"
  role = aws_iam_role.event_leads[each.key].id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = each.value
      Resource = [aws_dynamodb_table.event_leads.arn, "${aws_dynamodb_table.event_leads.arn}/index/*"]
    }]
  })
}

output "event_leads_table" {
  value = aws_dynamodb_table.event_leads.name
}

output "event_leads_role_arns" {
  value = { for component, role in aws_iam_role.event_leads : component => role.arn }
}
