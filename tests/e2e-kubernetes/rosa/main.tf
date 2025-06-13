terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 4.20.0"
    }

    rhcs = {
      version = ">= 1.6.8"
      source  = "terraform-redhat/rhcs"
    }
  }

  backend "s3" {}

  required_version = ">= 1.2.0"
}

provider "aws" {
  region  = var.aws_region
}

module "hcp" {
  source  = "terraform-redhat/rosa-hcp/rhcs"
  version = "1.6.8"
  
  openshift_version = "4.18.13"
  cluster_name = "test"
  compute_machine_type = "m5.xlarge"
  replicas = 2
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
  name = "${var.cluster_name}-openshift-credentials"
}

resource "aws_secretsmanager_secret_version" "secret_version" {
  secret_id     = aws_secretsmanager_secret.openshift_credentials.id
  secret_string = jsonencode({
    "openshift_username": module.hcp.cluster_admin_username
    "openshift_password": module.hcp.cluster_admin_password
    "openshift_server": module.hcp.cluster_api_url
  })
}

module "vpc" {
  source  = "terraform-redhat/rosa-hcp/rhcs//modules/vpc"

  name_prefix              = "${var.cluster_name}-vpc"
  azs             = ["us-east-1a"]
  cidr  = "10.0.0.0/16"
  private_subnets = ["10.0.1.0/24", "10.0.2.0/24", "10.0.3.0/24"]
  public_subnets  = ["10.0.101.0/24", "10.0.102.0/24", "10.0.103.0/24"]

  enable_nat_gateway   = true
  single_nat_gateway   = false
  enable_dns_hostnames = true
  enable_dns_support   = true
}