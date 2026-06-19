# Pulse on ECS Fargate: one task, an EFS volume for the SQLite file, an ALB in
# front. One task on purpose: SQLite wants a single writer, and a monitor
# that runs twice sends every alert twice.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = { source = "hashicorp/aws", version = "~> 5.60" }
  }
}

provider "aws" {
  region = var.region
}

data "aws_vpc" "this" { id = var.vpc_id }

resource "aws_cloudwatch_log_group" "pulse" {
  name              = "/ecs/pulse"
  retention_in_days = 30
}

resource "aws_ecs_cluster" "this" {
  name = "pulse"
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

# --- Storage ---------------------------------------------------------------

resource "aws_efs_file_system" "data" {
  encrypted        = true
  performance_mode = "generalPurpose"
  tags             = { Name = "pulse-data" }
}

resource "aws_efs_access_point" "data" {
  file_system_id = aws_efs_file_system.data.id
  posix_user {
    uid = 65532 # distroless nonroot
    gid = 65532
  }
  root_directory {
    path = "/pulse"
    creation_info {
      owner_uid   = 65532
      owner_gid   = 65532
      permissions = "750"
    }
  }
}

resource "aws_efs_mount_target" "data" {
  for_each        = toset(var.private_subnet_ids)
  file_system_id  = aws_efs_file_system.data.id
  subnet_id       = each.value
  security_groups = [aws_security_group.efs.id]
}

# --- Config ----------------------------------------------------------------

resource "aws_ssm_parameter" "config" {
  name  = "/pulse/config"
  type  = "SecureString"
  value = file(var.config_path)
}

# Values the config refers to as ${NAME}. Each becomes an environment
# variable in the task; Pulse refuses to start when the config references
# one that is missing, so an unsigned webhook cannot slip through.
locals {
  # The names are not secret, only the values; unwrapping the key set lets
  # them drive for_each without exposing a value.
  env_names = nonsensitive(toset([for k, v in var.secret_env : k if v != ""]))
}

resource "aws_ssm_parameter" "env" {
  for_each = local.env_names
  name     = "/pulse/env/${each.key}"
  type     = "SecureString"
  value    = var.secret_env[each.key]
}

# --- Network ---------------------------------------------------------------

resource "aws_security_group" "alb" {
  name   = "pulse-alb"
  vpc_id = data.aws_vpc.this.id
  ingress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "task" {
  name   = "pulse-task"
  vpc_id = data.aws_vpc.this.id
  ingress {
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }
  # Checks reach out to anything they are configured to probe.
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "efs" {
  name   = "pulse-efs"
  vpc_id = data.aws_vpc.this.id
  ingress {
    from_port       = 2049
    to_port         = 2049
    protocol        = "tcp"
    security_groups = [aws_security_group.task.id]
  }
}

resource "aws_lb" "this" {
  name               = "pulse"
  load_balancer_type = "application"
  subnets            = var.public_subnet_ids
  security_groups    = [aws_security_group.alb.id]
}

resource "aws_lb_target_group" "this" {
  name        = "pulse"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = data.aws_vpc.this.id
  # Only one task ever runs, so there are no other requests to drain for.
  # The default 300s left the page returning 503 for five minutes per deploy.
  deregistration_delay = 5
  health_check {
    path    = "/healthz"
    matcher = "200"
  }
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"
  certificate_arn   = var.certificate_arn
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.this.arn
  }
}

# --- Task ------------------------------------------------------------------

data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "pulse-execution"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "read_config" {
  statement {
    actions   = ["ssm:GetParameters"]
    resources = concat([aws_ssm_parameter.config.arn], [for p in aws_ssm_parameter.env : p.arn])
  }
}

resource "aws_iam_role_policy" "read_config" {
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.read_config.json
}

resource "aws_ecs_task_definition" "pulse" {
  family                   = "pulse"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512
  execution_role_arn       = aws_iam_role.execution.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  volume {
    name = "data"
    efs_volume_configuration {
      file_system_id     = aws_efs_file_system.data.id
      transit_encryption = "ENABLED"
      authorization_config {
        access_point_id = aws_efs_access_point.data.id
        iam             = "DISABLED"
      }
    }
  }

  container_definitions = jsonencode([{
    name      = "pulse"
    image     = var.image
    essential = true
    # The config arrives as PULSE_CONFIG from SSM; Pulse reads it from the
    # environment when set, so the distroless image needs no shell or file.
    command      = ["run"]
    portMappings = [{ containerPort = 8080, protocol = "tcp" }]
    environment  = [{ name = "PULSE_PUBLIC_URL", value = var.public_url }]
    secrets = concat(
      [{ name = "PULSE_CONFIG", valueFrom = aws_ssm_parameter.config.arn }],
      [for k, p in aws_ssm_parameter.env : { name = k, valueFrom = p.arn }],
    )
    mountPoints = [{ sourceVolume = "data", containerPath = "/data" }]
    stopTimeout = 20
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.pulse.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "pulse"
      }
    }
  }])
}

resource "aws_ecs_service" "pulse" {
  name            = "pulse"
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.pulse.arn
  desired_count   = 1
  launch_type     = "FARGATE"
  # Never two at once: stop the old task before starting the new one.
  deployment_minimum_healthy_percent = 0
  deployment_maximum_percent         = 100

  network_configuration {
    subnets          = var.private_subnet_ids
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = false
  }
  load_balancer {
    target_group_arn = aws_lb_target_group.this.arn
    container_name   = "pulse"
    container_port   = 8080
  }
  depends_on = [aws_lb_listener.https, aws_efs_mount_target.data]
}
