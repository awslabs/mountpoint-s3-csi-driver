#!/usr/bin/env -S uv run --script
#
# /// script
# requires-python = ">=3.12"
# dependencies = ["boto3"]
# ///

"""
DynamoDB-based workflow serialization.
Enqueues the workflow and waits until it's at the front of the queue.

The table consists of queue positions.
The entry at the front holds the right to access the resources within the account.
Entries have a primary/partition key of the enqueue timestamp, such that order is maintained.

When at the front of the queue,
the workflow updates its entry with a long TTL and will be removed when the workflow exits.
For entries later in the queue, a short TTL is used.
This means if a job exits uncleanly, the queue can self-recover.
Jobs waiting in the queue regularly update the short TTL to stay active.
"""

import sys
import time
import subprocess
from datetime import datetime, timezone, timedelta
import boto3
from botocore.exceptions import ClientError
import argparse

POLL_INTERVAL = timedelta(minutes=1)
MAX_QUEUE_DUR = timedelta(hours=5, minutes=30)
# TTL that exceeds expected runtime of workflows.
RUNNING_TTL = timedelta(hours=8)
# Short TTL since we refresh while actively waiting
WAITING_TTL = timedelta(minutes=10)

def main():
    parser = argparse.ArgumentParser()
    subparser = parser.add_subparsers(dest='command', required=True)

    enqueue_parser = subparser.add_parser("enqueue")
    enqueue_parser.add_argument('--table-name', required=True)
    enqueue_parser.add_argument('--workflow-run-id', required=True)
    enqueue_parser.add_argument('--github-repository', required=False)

    dequeue_parser = subparser.add_parser("dequeue")
    dequeue_parser.add_argument('--table-name', required=True)
    dequeue_parser.add_argument('--workflow-run-id', required=True)

    args = parser.parse_args()

    dynamodb = boto3.client('dynamodb')

    match args.command:
        case "enqueue":
            enqueue_and_wait(dynamodb, args.table_name, args.workflow_run_id, args.github_repository)
        case "dequeue":
            dequeue(dynamodb, args.table_name, args.workflow_run_id)
        case _:
            assert False, "unreachable"


def restart_workflow(github_repo, workflow_run_id) -> bool:
    """
    Restart the GitHub workflow using GH CLI.
    Return value indicates if a restart was issued succesfully.
    """
    if not github_repo:
        print("No GitHub repository provided, skipping restart")
        return False

    try:
        print(f"Restarting workflow run {workflow_run_id} in {github_repo}...")
        _ = subprocess.run([
            "gh", "api", f"repos/{github_repo}/actions/runs/{workflow_run_id}/rerun",
            "--method", "POST"
        ], check=True, capture_output=True, text=True)

        print(f"Successfully restarted workflow run {workflow_run_id}")
        return True
    except subprocess.CalledProcessError as e:
        print(f"Failed to restart workflow: {e}")
        print(f"Error output: {e.stderr}")
        return False
    except FileNotFoundError:
        print("Error: 'gh' CLI not found. Cannot restart workflow.")
        return False


def enqueue_and_wait(dynamodb, table_name, workflow_run_id, github_repo=None):
    print("Enqueuing workflow")
    print(f"  Table: {table_name}")
    print(f"  Workflow run: {workflow_run_id}")
    print()

    now = datetime.now(timezone.utc)
    waiting_ttl = int((now + WAITING_TTL).timestamp())

    # Enqueue the workflow
    try:
        item = {
            'workflow_run_id': {'S': workflow_run_id},
            'enqueued_at': {'S': now.isoformat()},
            'ttl': {'N': str(waiting_ttl)}
        }

        dynamodb.put_item(
            TableName=table_name,
            Item=item,
            ConditionExpression='attribute_not_exists(workflow_run_id)'
        )
        print(f"Successfully enqueued workflow {workflow_run_id}")
        print()
    except ClientError as e:
        if e.response['Error']['Code'] == 'ConditionalCheckFailedException':
            print(f"Workflow {workflow_run_id} already exists in queue, ignoring error")
            print()
        else:
            print(f"Failed to enqueue workflow: {e}")
            sys.exit(1)

    # Wait for our turn
    start_time = time.time()

    while True:
        elapsed = int(time.time() - start_time)

        if elapsed >= MAX_QUEUE_DUR.seconds:
            print(f"Approaching GitHub timeout after {elapsed}s waiting for queue position")
            if github_repo:
                print("Attempting to restart workflow before timeout...")
                if restart_workflow(github_repo, workflow_run_id):
                    print("Workflow restart initiated, exiting")
                else:
                    print("Failed to restart workflow, exiting after timeout")
            else:
                print("No GitHub repository provided for restart, exiting without restart")
            sys.exit(1)

        print(f"Checking queue position (elapsed: {elapsed}s)...")

        try:
            # Get all workflows in queue (excluding expired TTLs)
            now_timestamp = int(datetime.now(timezone.utc).timestamp())
            response = dynamodb.scan(
                TableName=table_name,
                ConsistentRead=True,
                FilterExpression='#ttl > :now_timestamp',
                ExpressionAttributeNames={
                    '#ttl': 'ttl'
                },
                ExpressionAttributeValues={
                    ':now_timestamp': {'N': str(now_timestamp)}
                }
            )

            all_workflows = response.get('Items', [])

            if not all_workflows:
                print("No workflows in queue, expected current job to be in the queue, exiting")
                sys.exit(1)

            # Sort by enqueued_at timestamp
            all_workflows.sort(key=lambda x: x['enqueued_at']['S'])
            print(f"Queue: {[w['workflow_run_id']['S'] for w in all_workflows]}")

            first_workflow = all_workflows[0]
            first_workflow_id = first_workflow['workflow_run_id']['S']

            for idx, item in enumerate(all_workflows):
                if item['workflow_run_id']['S'] == workflow_run_id:
                    queue_position = idx + 1
                    break

            print(f"First in queue: {first_workflow_id}")
            print(f"Queue position: {queue_position}")

            if first_workflow_id == workflow_run_id:
                print("This workflow is at the front of the queue!")

                # Update TTL to running timeout and proceed
                if start_running(dynamodb, table_name, workflow_run_id):
                    print(f"Updated TTL to exceed expected max workflow duration ({MAX_QUEUE_DUR.seconds}s)")
                    return
                else:
                    print("Failed to update TTL (race condition), retrying...")
            else:
                print(f"Not our turn yet, waiting {POLL_INTERVAL.seconds}s...")
                # Refresh TTL to keep our item alive while actively waiting
                refresh_ttl(dynamodb, table_name, workflow_run_id)

        except ClientError as e:
            print(f"Error checking queue: {e}")

        time.sleep(POLL_INTERVAL.seconds)


def refresh_ttl(dynamodb, table_name, workflow_run_id):
    """
    Refresh TTL to keep workflow alive while actively waiting.
    """
    try:
        now = datetime.now(timezone.utc)
        waiting_ttl = int((now + WAITING_TTL).timestamp())

        dynamodb.update_item(
            TableName=table_name,
            Key={'workflow_run_id': {'S': workflow_run_id}},
            UpdateExpression='SET #ttl = :new_ttl',
            ExpressionAttributeNames={
                '#ttl': 'ttl'
            },
            ExpressionAttributeValues={
                ':new_ttl': {'N': str(waiting_ttl)}
            }
        )
        print(f"Refreshed TTL for workflow {workflow_run_id}")
    except ClientError as e:
        print(f"Warning: Failed to refresh TTL: {e}")


def start_running(dynamodb, table_name, workflow_run_id):
    """
    Update TTL to longer timeout for running workflow.
    Returns True if successful, False if there was a race condition.
    """
    try:
        now = datetime.now(timezone.utc)
        running_ttl = int((now + RUNNING_TTL).timestamp())

        dynamodb.update_item(
            TableName=table_name,
            Key={'workflow_run_id': {'S': workflow_run_id}},
            UpdateExpression='SET #ttl = :new_ttl',
            ExpressionAttributeNames={
                '#ttl': 'ttl'
            },
            ExpressionAttributeValues={
                ':new_ttl': {'N': str(running_ttl)}
            }
        )
        return True
    except ClientError as e:
        print(f"Error updating TTL for running: {e}")
        return False


def dequeue(dynamodb, table_name, workflow_run_id):
    print("Removing workflow from queue")
    print(f"  Table: {table_name}")
    print(f"  Workflow run: {workflow_run_id}")

    try:
        dynamodb.delete_item(
            TableName=table_name,
            Key={'workflow_run_id': {'S': workflow_run_id}}
        )
        print("Successfully removed workflow from queue")
    except ClientError as e:
        print(f"Warning: Failed to remove workflow from queue: {e}")


if __name__ == '__main__':
    main()
