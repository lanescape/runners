output "service_url" {
  description = "The service's URL, and the audience its callers' tokens name."
  value       = local.service_url
}

output "proxy_url" {
  description = "The HTTP proxy's URL, and the audience its callers' tokens name."
  value       = local.proxy_url
}

output "proxy_git_address" {
  description = "The git machine's internal address, which only the service's machines can reach, on port 443."
  value       = local.proxy_git_address
}
