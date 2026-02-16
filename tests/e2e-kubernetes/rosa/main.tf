terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.30.0"
    }

    rhcs = {
      version = ">= 1.7.1"
      source  = "terraform-redhat/rhcs"
    }
  }

  backend "s3" {}

  required_version = ">= 1.14.4"
}

provider "aws" {
  region  = var.aws_region
}

provider "rhcs" {
  token = var.rhcs_token
}

data "aws_caller_identity" "current" {}

# Capture cluster creation timestamp in state
# Used in e2e-rosa-tests.yaml to evaluate whether we should re-create cluster if it's too old
resource "terraform_data" "cluster_creation_time" {
  input = timestamp()

  lifecycle {
    ignore_changes = [input]
  }

  depends_on = [module.hcp]
}

module "hcp" {
  source  = "terraform-redhat/rosa-hcp/rhcs"
  version = "1.7.1"

  openshift_version = var.openshift_version
  cluster_name = var.cluster_name
  compute_machine_type = var.compute_machine_type
  replicas = var.replicas
  machine_cidr = module.vpc.cidr_block
  aws_availability_zones = module.vpc.availability_zones
  aws_subnet_ids = concat(module.vpc.public_subnets, module.vpc.private_subnets)
  create_oidc = true
  create_admin_user = true
  create_account_roles = true
  create_operator_roles = true
  aws_billing_account_id = var.billing_account_id
  ec2_metadata_http_tokens = "required"
}

resource "aws_secretsmanager_secret" "openshift_credentials" {
  name                    = "${var.cluster_name}-openshift-credentials"
  recovery_window_in_days = 0 # Force immediate deletion to avoid conflicts in subsequent CI runs
}

resource "aws_secretsmanager_secret_version" "secret_version" {
  secret_id     = aws_secretsmanager_secret.openshift_credentials.id
  secret_string = jsonencode({
    "cluster_admin_username": module.hcp.cluster_admin_username
    "cluster_admin_password": module.hcp.cluster_admin_password
    "cluster_api_url": module.hcp.cluster_api_url
  })
}

module "vpc" {
  source  = "terraform-redhat/rosa-hcp/rhcs//modules/vpc"

  name_prefix              = "${var.cluster_name}-vpc"
  availability_zones_count = 3
}

resource "aws_iam_role_policy_attachment" "ecr_readonly_worker_attachment" {
  role       = "${var.cluster_name}-account-HCP-ROSA-Worker-Role"
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"

  depends_on = [module.hcp]
}

resource "aws_iam_role" "csi_driver_irsa" {
  name = "${var.cluster_name}-driver-irsa"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Principal = {
          Federated = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:oidc-provider/${replace(module.hcp.oidc_endpoint_url, "https://", "")}"
        }
        Action = "sts:AssumeRoleWithWebIdentity"
        Condition = {
          StringEquals = {
            "${replace(module.hcp.oidc_endpoint_url, "https://", "")}:sub" = "system:serviceaccount:kube-system:s3-csi-driver-sa"
          }
        }
      }
    ]
  })
}

resource "aws_iam_policy" "csi_driver_s3_express_policy" {
  name        = "${var.cluster_name}-s3-express-policy"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "s3express:*",
        ]
        Resource = "*"
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "csi_driver_s3_full_access" {
  role       = aws_iam_role.csi_driver_irsa.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonS3FullAccess"
}

resource "aws_iam_role_policy_attachment" "csi_driver_s3_express_full_access" {
  role       = aws_iam_role.csi_driver_irsa.name
  policy_arn = aws_iam_policy.csi_driver_s3_express_policy.arn
}
