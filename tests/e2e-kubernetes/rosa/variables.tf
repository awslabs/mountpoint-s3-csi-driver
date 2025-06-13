variable "rhcs_token" {
  type = string
  sensitive = true
}

variable "billing_account_id" {
  type = string
  sensitive = true
}

variable "cluster_name" {
  type = string
}

variable "aws_region" {
  type = string
}
