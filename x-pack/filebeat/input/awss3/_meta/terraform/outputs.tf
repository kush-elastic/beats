resource "local_file" "secrets" {
  content = yamlencode({
    "queue_url" : aws_sqs_queue.filebeat-integtest.url
    "aws_region" : aws_s3_bucket.filebeat-integtest.region
    "bucket_name" : aws_s3_bucket.filebeat-integtest.id
    "bucket_name_for_sns" : aws_s3_bucket.filebeat-integtest-sns.id
    "queue_url_for_sns" : aws_sqs_queue.filebeat-integtest-sns.url
    "bucket_name_for_eventbridge" : aws_s3_bucket.filebeat-integtest-eventbridge.id
    "queue_url_for_eventbridge" : aws_sqs_queue.filebeat-integtest-eventbridge.url
  })
  filename        = "${path.module}/outputs.yml"
  file_permission = "0644"
}

resource "local_file" "secrets-localstack" {
  content = yamlencode({
    "queue_url" : aws_sqs_queue.filebeat-integtest-localstack.url
    "aws_region" : aws_s3_bucket.filebeat-integtest-localstack.region
    "bucket_name" : aws_s3_bucket.filebeat-integtest-localstack.id
  })
  filename        = "${path.module}/outputs-localstack.yml"
  file_permission = "0644"
}
