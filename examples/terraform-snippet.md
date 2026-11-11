# Pointing an upstream URL at the proxy from Terraform

Integration with Faultline is one configuration value per dependency: the URL your service uses to reach it. Make that URL a variable, and switch it per environment.

The example is an ECS service on AWS; the same idea applies to any platform that sets environment variables.

```hcl
variable "environment" {
  type = string
}

variable "faultline_proxy_url" {
  description = "Base URL of the Faultline proxy traffic port. Empty disables it."
  type        = string
  default     = ""
}

locals {
  inventory_direct = "http://inventory.${var.environment}.internal:8080"

  # Only non-production environments may route through the proxy.
  use_faultline = var.faultline_proxy_url != "" && !contains(["prod", "production", "live"], lower(var.environment))

  # The route name (inventory) must match a route in faultline.yaml whose
  # upstream is the direct URL above.
  inventory_url = local.use_faultline ? "${var.faultline_proxy_url}/inventory" : local.inventory_direct
}

resource "aws_ecs_task_definition" "orders" {
  family                   = "orders-${var.environment}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = 256
  memory                   = 512

  container_definitions = jsonencode([
    {
      name  = "orders"
      image = "registry.example.test/orders:latest"
      environment = [
        { name = "INVENTORY_URL", value = local.inventory_url }
      ]
    }
  ])
}
```

In the QA workspace:

```hcl
environment         = "qa"
faultline_proxy_url = "http://faultline-proxy.qa.internal:7701"
```

And the matching route in `faultline.yaml`:

```yaml
routes:
  - name: inventory
    upstream: http://inventory.qa.internal:8080
```

Things to keep in mind:

- With no experiment running, the proxy passes every request through unchanged, so leaving the QA services pointed at it permanently is fine.
- The proxy's traffic port has no authentication. Restrict its security group to the services that use it.
- Never set `faultline_proxy_url` in a production workspace. The `use_faultline` guard above and Faultline's own environment guard are two separate checks; keep both.
- The service still has to forward `X-Chaos-Run` and `X-Chaos-Correlation` on its outgoing calls for faults to reach requests that entered through another service.
