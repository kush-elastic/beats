terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "4.46.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
  default_tags {
    tags = {
      environment  = var.ENVIRONMENT
      repo         = var.REPO
      branch       = var.BRANCH
      build        = var.BUILD_ID
      created_date = var.CREATED_DATE
      division     = "engineering"
      org          = "obs"
      team         = "cloud-monitoring"
      project      = "filebeat_aws-ci"
    }
  }
}

resource "random_string" "random" {
  length  = 6
  special = false
  upper   = false
}

resource "aws_s3_bucket" "filebeat-integtest" {
  bucket        = "filebeat-s3-integtest-${random_string.random.result}"
  force_destroy = true
}

resource "aws_sqs_queue" "filebeat-integtest" {
  name   = "filebeat-s3-integtest-${random_string.random.result}"
  policy = <<POLICY
{
  "Version": "2012-10-17",
  "Id": "sqspolicy",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": "*",
      "Action": "sqs:SendMessage",
      "Resource": "arn:aws:sqs:*:*:filebeat-s3-integtest-${random_string.random.result}",
      "Condition": {
        "ArnEquals": { "aws:SourceArn": "${aws_s3_bucket.filebeat-integtest.arn}" }
      }
    }
  ]
}
POLICY

  depends_on = [
    aws_s3_bucket.filebeat-integtest,
  ]
}

resource "aws_s3_bucket_notification" "bucket_notification" {
  bucket = aws_s3_bucket.filebeat-integtest.id

  queue {
    queue_arn = aws_sqs_queue.filebeat-integtest.arn
    events    = ["s3:ObjectCreated:*"]
  }

  depends_on = [
    aws_s3_bucket.filebeat-integtest,
    aws_sqs_queue.filebeat-integtest,
  ]
}

resource "aws_sns_topic" "filebeat-integtest-sns" {
  name = "filebeat-s3-integtest-sns-${random_string.random.result}"

  policy = <<POLICY
{
    "Version":"2012-10-17",
    "Statement":[{
        "Effect": "Allow",
        "Principal": { "Service": "s3.amazonaws.com" },
        "Action": "SNS:Publish",
        "Resource": "arn:aws:sns:*:*:filebeat-s3-integtest-sns-${random_string.random.result}",
        "Condition":{
            "ArnEquals": { "aws:SourceArn": "${aws_s3_bucket.filebeat-integtest-sns.arn}" }
        }
    }]
}
POLICY

  depends_on = [
    aws_s3_bucket.filebeat-integtest-sns,
  ]
}

resource "aws_s3_bucket" "filebeat-integtest-sns" {
  bucket        = "filebeat-s3-integtest-sns-${random_string.random.result}"
  force_destroy = true
}

resource "aws_s3_bucket_notification" "bucket_notification-sns" {
  bucket = aws_s3_bucket.filebeat-integtest-sns.id

  topic {
    topic_arn = aws_sns_topic.filebeat-integtest-sns.arn
    events    = ["s3:ObjectCreated:*"]
  }

  depends_on = [
    aws_s3_bucket.filebeat-integtest-sns,
    aws_sns_topic.filebeat-integtest-sns,
  ]
}

resource "aws_sqs_queue" "filebeat-integtest-sns" {
  name = "filebeat-s3-integtest-sns-${random_string.random.result}"

  policy = <<POLICY
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": "*",
      "Action": "sqs:SendMessage",
      "Resource": "arn:aws:sqs:*:*:filebeat-s3-integtest-sns-${random_string.random.result}",
      "Condition": {
        "ArnEquals": { "aws:SourceArn": "${aws_sns_topic.filebeat-integtest-sns.arn}" }
      }
    }
  ]
}
POLICY

  depends_on = [
    aws_s3_bucket.filebeat-integtest-sns,
    aws_sns_topic.filebeat-integtest-sns
  ]
}

resource "aws_sns_topic_subscription" "filebeat-integtest-sns" {
  topic_arn = aws_sns_topic.filebeat-integtest-sns.arn
  protocol  = "sqs"
  endpoint  = aws_sqs_queue.filebeat-integtest-sns.arn
}

resource "aws_s3_bucket" "filebeat-integtest-eventbridge" {
  bucket        = "filebeat-s3-integtest-eventbridge-${random_string.random.result}"
  force_destroy = true
}

resource "aws_sqs_queue" "filebeat-integtest-eventbridge" {
  name = "filebeat-s3-integtest-eventbridge-${random_string.random.result}"
}

data "aws_iam_policy_document" "sqs_queue_policy" {
  statement {
    effect  = "Allow"
    actions = ["sqs:SendMessage"]

    principals {
      type        = "Service"
      identifiers = ["events.amazonaws.com"]
    }

    resources = [aws_sqs_queue.filebeat-integtest-eventbridge.arn]
  }
}

resource "aws_sqs_queue_policy" "filebeat-integtest-eventbridge" {
  queue_url = aws_sqs_queue.filebeat-integtest-eventbridge.id
  policy    = data.aws_iam_policy_document.sqs_queue_policy.json
}

resource "aws_cloudwatch_event_rule" "sqs" {
  name        = "capture-s3-notification"
  description = "Capture s3 changes"

  event_pattern = jsonencode({
      source = [
          "aws.s3"
      ],
      detail-type = [
          "Object Created"
      ]
      detail = {
        bucket = {
            name = [ aws_s3_bucket.filebeat-integtest-eventbridge.id ]
        }
      }
  })

  depends_on = [
      aws_s3_bucket.filebeat-integtest-eventbridge
  ]
}

resource "aws_cloudwatch_event_target" "sqs" {
  rule      = aws_cloudwatch_event_rule.sqs.name
  target_id = "SendToSQS"
  arn       = aws_sqs_queue.filebeat-integtest-eventbridge.arn

  depends_on = [
   aws_cloudwatch_event_rule.sqs
  ]
}

resource "aws_s3_bucket_notification" "bucket_notification-eventbridge" {
  bucket = aws_s3_bucket.filebeat-integtest-eventbridge.id
  eventbridge = true

  depends_on = [
    aws_cloudwatch_event_target.sqs
  ]
}

