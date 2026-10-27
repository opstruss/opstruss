variable "name" {
  type = string
}

resource "terraform_data" "site" {
  input = var.name
}

output "url" {
  value = "https://${var.name}.example.com"
}
