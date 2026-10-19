# Region to deploy into
variable "aws_region" {
  type    = string
  default = "us-east-1"
}
# ECR settings
variable "ecr_repository_name" {
  type    = string
  default = "product-service"
}

variable "service_name" {
  type    = string
  default = "product-api"
}

variable "container_port" {
  type    = number
  default = 8080
}

variable "ecs_count" {
  type    = number
  default = 1
}

# How long to keep logs
variable "log_retention_days" {
  type    = number
  default = 7
}
