# Mountpoint for Amazon S3 File System Behavior

The Scality S3 CSI Driver uses Mountpoint for Amazon S3 to present S3-compatible buckets as filesystems.
All file system semantics are inherited from Mountpoint for Amazon S3, and are compatible with Scality S3 and any S3-compatible storage.
This documentation describes only the behavior relevant to S3 Standard buckets.
Features specific to AWS services other than S3, such as S3 Express One Zone, Glacier, or Archive storage classes, are not supported nor relevant.

## Behavior Tenets

1. Mountpoint does not support file behaviors that cannot be implemented efficiently against S3's object APIs.
   It does not emulate operations like `rename` that would require many API calls to S3 to perform.
2. Mountpoint presents a common view of S3 object data through both file and object APIs.
   It does not emulate POSIX file features that have no close analog in S3's object APIs, such as mutable ownership and permissions.
3. When these tenets conflict with POSIX requirements, Mountpoint fails early and explicitly.
   It will cause end user applications to fail with IO errors rather than silently accept operations that will never succeed, such as extended attributes.

## Reading and Writing Files

- Supports opening and reading existing objects, optimized for large sequential reads.
- Supports random reads and seeking within existing objects.
- Supports creating new objects by writing new files.
- With `--allow-overwrite`, supports replacing existing objects (must use `O_TRUNC` to truncate).
- Writes must always start from the beginning and be sequential.
- Uploads are asynchronous and optimized for throughput; use `fsync` to guarantee upload before closing.
- Cannot continue writing after `fsync`.
- New or overwritten objects are visible to other S3 clients only after closing or `fsync`.
- By default, deleting objects (`rm`) is not allowed; enable with `--allow-delete`.
- Delete operations immediately remove the object from S3.
- Cannot delete a file while it is being written.
- Renaming files is not supported.

## Directories

- S3 is flat; Mountpoint infers directories from `/` in object keys.
- Not all S3 object keys correspond to valid file names; some objects may not be accessible.
- If a directory and file share a name, only the directory is accessible.
- Creating directories (`mkdir`) is local and not persisted to S3 until a file is written inside.
- Cannot remove or rename existing directories; can remove new local directories if empty.
- No support for hard or symbolic links.

## Permissions and Metadata

- By default, files/directories are readable only by the mounting user.
- Use `--allow-other` to allow access by other users.
- Default permissions and owners can be overridden at mount time with `--uid`, `--gid`, `--file-mode`, `--dir-mode`.
- Permissions are emulated and cannot be changed after mount.
- Mountpoint respects S3 IAM, bucket policies, and ACLs.
- Limited support for file metadata (modification times, sizes); cannot modify metadata.

## Consistency and Concurrency

- S3 provides strong read-after-write consistency for PUT and DELETE.
- Mountpoint provides strong read-after-write consistency for file writes, directory listings, and new object creation.
- Modifying/deleting an object with another client may result in stale metadata for up to 1 second.
- Directory listings are always up-to-date.
- Multiple readers can access the same object, but only one writer can do so at a time.
- Files being written are not available for reading until closed.
- No coordination between multiple Mountpoint mounts for the same bucket; do not write to the same object from multiple instances.
- New file uploads are atomic by default.

## Caching

- Optional metadata and object content caching is available.
- With caching, strong consistency is relaxed; may see stale data for up to the cache's TTL.
- Use `O_DIRECT` to force up-to-date reads.
- Caching does not affect write behavior; files being written are unavailable until closed.

## Durability

- Mountpoint translates file operations into S3 API calls, relying on S3's data integrity mechanisms.
- For end-to-end integrity, use SDKs and checksums.

## Error Handling

- File operations may fail due to network or S3 errors; Mountpoint uses retries and backoff.
- Use `fsync` to ensure files are uploaded; errors on `fsync` mean the file may not be uploaded.

## Detailed Semantics

### Mapping S3 Object Keys to Files and Directories

- Keys are split on `/` to form file system paths.
- Some S3 keys (null bytes, `.`/`..`, trailing `/`, conflicts) are not accessible.
- Directories shadow files of the same name.
- Remote directories shadow local directories/files.
- Windows-style path delimiters (`\`) are not supported.

### File Operations

- **Reads**: Fully supported, including sequential and random reads.
- **Writes**: Sequential only; must start at beginning. Overwrites require `O_TRUNC` and `--allow-overwrite`.
- **Deletes**: Enabled with `--allow-delete`; immediate removal from S3.
- **Renames**: Not supported.
- **Directory operations**: Read-only operations supported; `mkdir` is local until a file is written. `rmdir` only deletes empty local directories.
- **Metadata**: Reading supported with limitations; modifying not supported.
- **Links**: Hard and symbolic links are not supported.

### Consistency

- Strong read-after-write for new objects and writes.
- Stale metadata possible for up to 1 second after concurrent modification/deletion by another client.

For more details, see the [Mountpoint for Amazon S3 documentation](https://github.com/awslabs/mountpoint-s3/blob/main/doc/SEMANTICS.md).
