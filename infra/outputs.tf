output "status_page_dns" {
  description = "Point the status page's domain at this with a CNAME or alias record."
  value       = aws_lb.this.dns_name
}

output "efs_id" {
  value = aws_efs_file_system.data.id
}
