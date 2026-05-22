variable "region" {
  type    = string
  default = "us-east-1"
}

variable "vpc_id" {
  type = string
}

variable "public_subnet_ids" {
  description = "Subnets for the load balancer."
  type        = list(string)
}

variable "private_subnet_ids" {
  description = "Subnets for the task and the EFS mount targets. They need a NAT route for the checks to reach the internet."
  type        = list(string)
}

variable "certificate_arn" {
  description = "ACM certificate for the status page's domain."
  type        = string
}

variable "image" {
  description = "Container image, e.g. <account>.dkr.ecr.<region>.amazonaws.com/pulse:1.0.0"
  type        = string
}

variable "config_path" {
  description = "Local path to the pulse.yaml to store in SSM."
  type        = string
  default     = "../examples/pulse.yaml"
}

variable "public_url" {
  description = "Public URL of the status page, used in notification links."
  type        = string
}
