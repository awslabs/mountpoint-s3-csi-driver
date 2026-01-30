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

provider "rhcs" {
  token = var.rhcs_token
}

module "hcp" {
  source  = "terraform-redhat/rosa-hcp/rhcs"
  version = "1.6.8"

  openshift_version = "4.18.13"
  cluster_name = "${var.cluster_name}"
  compute_machine_type = "m5.xlarge"
  replicas = 3
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

module "vpc" {
  source  = "terraform-redhat/rosa-hcp/rhcs//modules/vpc"

  name_prefix              = "${var.cluster_name}-vpc"
  availability_zones_count = 3
}