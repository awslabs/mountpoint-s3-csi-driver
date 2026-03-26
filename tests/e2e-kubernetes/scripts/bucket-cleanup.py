#!/usr/bin/env -S uv run --script
# /// script
# dependencies = [
#   "boto3>=1.34.0",
# ]
# ///

import argparse
import boto3
from datetime import datetime, timezone, timedelta
from concurrent.futures import ThreadPoolExecutor, as_completed

def delete_bucket_objects(s3, bucket_name):
    """Delete all objects in a bucket using pagination"""
    paginator = s3.get_paginator('list_objects_v2')
    count = 0
    for page in paginator.paginate(Bucket=bucket_name, PaginationConfig={'PageSize': 1000}):
        if 'Contents' in page:
            keys = [obj['Key'] for obj in page['Contents']]
            count += len(keys)
            s3.delete_objects(Bucket=bucket_name, Delete={'Objects': [{'Key': key} for key in keys]})
    print(f"Deleted {count} objects from {bucket_name}")

def delete_bucket(s3, bucket_name, created, dry_run=False):
    """Delete a single bucket and all its contents"""
    try:
        if dry_run:
            print(f"[DRY RUN] Would delete bucket: {bucket_name} (created {created.isoformat()})")
            return True
        print(f"Deleting bucket: {bucket_name} (created {created.isoformat()})")
        delete_bucket_objects(s3, bucket_name)
        s3.delete_bucket(Bucket=bucket_name)
        print(f"Bucket deleted successfully: {bucket_name}")
        return True
    except Exception as e:
        print(f"  Failed to delete {bucket_name}: {e}")
        return False

def delete_single(args):
    """Delete a single bucket by name"""
    s3 = boto3.client('s3')
    delete_bucket(s3, args.bucket, datetime.now(timezone.utc), dry_run=False)

def cleanup_old(args):
    """Clean up old buckets with prefix"""
    cutoff = datetime.now(timezone.utc) - timedelta(days=args.days_old)
    s3 = boto3.client('s3')

    mode = "[DRY RUN] " if args.dry_run else ""
    print(f"{mode}Deleting buckets with prefix '{args.prefix}' older than {args.days_old} days (before {cutoff.isoformat()})")

    # Standard S3 buckets
    standard_total = standard_deleted = 0
    try:
        buckets_to_delete = []
        for bucket in s3.list_buckets()['Buckets']:
            name = bucket['Name']
            if name.startswith(args.prefix):
                standard_total += 1
                if bucket['CreationDate'] < cutoff:
                    buckets_to_delete.append((name, bucket['CreationDate']))

        with ThreadPoolExecutor(max_workers=args.threads) as executor:
            futures = {executor.submit(delete_bucket, s3, name, created, args.dry_run): name for name, created in buckets_to_delete}
            for future in as_completed(futures):
                if future.result():
                    standard_deleted += 1
    except Exception as e:
        print(f"Error processing standard buckets: {e}")

    print(f"General purpose buckets: total {standard_total}, deleted {standard_deleted}")

    # S3 Express buckets
    express_total = express_deleted = 0
    try:
        buckets_to_delete = []
        for bucket in s3.list_directory_buckets()['Buckets']:
            name = bucket['Name']
            if name.startswith(args.prefix):
                express_total += 1
                if bucket['CreationDate'] < cutoff:
                    buckets_to_delete.append((name, bucket['CreationDate']))

        with ThreadPoolExecutor(max_workers=args.threads) as executor:
            futures = {executor.submit(delete_bucket, s3, name, created, args.dry_run): name for name, created in buckets_to_delete}
            for future in as_completed(futures):
                if future.result():
                    express_deleted += 1
    except Exception as e:
        print(f"Error processing Express buckets: {e}")

    print(f"Directory buckets: total with prefix {express_total}, deleted {express_deleted}")

def main():
    parser = argparse.ArgumentParser(description='Empty and delete S3 buckets')

    subparsers = parser.add_subparsers(dest='command', required=True, help='Command to run')

    # Single bucket deletion
    single_parser = subparsers.add_parser('delete', help='Delete a single bucket')
    single_parser.add_argument('bucket', help='Bucket name to delete')
    single_parser.set_defaults(func=delete_single)

    # Cleanup old buckets
    cleanup_parser = subparsers.add_parser('cleanup', help='Delete old buckets with prefix')
    cleanup_parser.add_argument('--days-old', type=int, default=7, help='Delete buckets older than N days (default: 7)')
    cleanup_parser.add_argument('--prefix', default='s3-csi-k8s-e2e-', help='Bucket name prefix to match')
    cleanup_parser.add_argument('--threads', type=int, default=10, help='Number of parallel threads (default: 10)')
    cleanup_parser.add_argument('--dry-run', action='store_true', help='Show what would be deleted without deleting')
    cleanup_parser.set_defaults(func=cleanup_old)

    args = parser.parse_args()
    args.func(args)

if __name__ == '__main__':
    main()
