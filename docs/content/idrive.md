---
title: "IDrive Cloud Drive"
description: "Experimental IDrive Cloud Drive backend"
---

This experimental backend accesses IDrive Cloud Drive (Sync storage) through
the web console's private HTTP endpoints. IDrive e2 uses the S3 backend instead.
IDrive Cloud Backup device folders are not supported by this backend.

## Configuration

Use `rclone config` to create an `idrive` remote. Supply the HTTPS origin of
your Cloud Drive server and an existing Cloud Drive web session token. These
are available in a Cloud Drive `browseFolder` request in your browser's
developer tools. Enter the token at the interactive password prompt.

Automatic sign-in and session renewal are not implemented. An expired token
must be replaced using `rclone config`. Do not commit HAR captures or tokens.

## Supported operations

The initial implementation supports directory listings, downloads, creating
directories, and uploads using the web console's single-request and chunked
protocols. File removal and
empty directory removal move items to IDrive Trash. Permanent deletion is not
implemented.

Source modification times are not preserved. IDrive skips same-size uploads
when the timestamp is unchanged, even if their contents differ. The backend
assigns an upload timestamp distinct from the existing file and reports
modification-time preservation as unsupported.

Without remote checksums or preserved timestamps, rclone may compare files by
size alone. Use `--ignore-times` to force replacements of same-size files.

Uploads of unknown size, server-side copy/move,
metadata-only modification-time changes, and remote checksums are unsupported.
Range reads download and discard the preceding bytes, so seeking in a large
file can be slow. Restricted names use reversible filename encoding. Directory periods are
escaped separately because IDrive rejects them; ordinary file extensions
are retained. Encoded names may appear differently in IDrive's web console.
The server's `lmd_web` timestamp takes precedence over its
formatted `lmd` timestamp.

HTTP requests currently have a two-minute total timeout, including download
streaming. Integration with rclone's timeout and retry settings is unfinished.

This is a prototype for protocol validation. It is not yet an upstream-ready
backend. Live standard backend integration tests and chunked transfer
round-trips pass. Automatic authentication, session renewal, and HTTP transport
integration remain unfinished. The private API may change.
