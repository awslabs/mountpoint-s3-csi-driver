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

variable "openshift_version" {
  type    = string
  default = "4.20.12"
}

variable "compute_machine_type" {
  type    = string
  default = "m5.xlarge"
}

variable "replicas" {
  type    = number
  default = 3
}
