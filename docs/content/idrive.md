---
title: "IDrive Cloud Drive"
description: "Experimental IDrive Cloud Drive backend"
---

This experimental backend accesses IDrive Cloud Drive (Sync storage) through
the web console's private HTTP endpoints. IDrive e2 uses the S3 backend instead.
IDrive Cloud Backup device folders are not supported by this backend.

## Configuration

Use `rclone config` to create an `idrive` remote and choose browser authorization.
Rclone opens the official Cloud Drive page. Sign in there and complete any MFA,
captcha, or private-key unlock in the browser. Then choose either:

- **HAR import:** open DevTools -> Network, refresh Cloud Drive, and export a
  local HAR including sensitive headers. Enter its absolute path in rclone.
  In Chrome/Edge, enable DevTools Settings -> Preferences -> Network ->
  "Allow to generate HAR with sensitive data" first.
- **Masked token prompt:** in the signed-in tab's DevTools Console run
  `copy(localStorage.getItem("Token"))`, then paste at rclone's masked prompt.

Setup discovers the storage server, obtains a Cloud Drive token, and validates
access with a root listing. The account password and private encryption key
stay in the browser; rclone saves reusable session tokens. HAR captures contain
credentials and must remain local. After import, the capture is no longer needed.
Rclone obscures saved tokens; use encrypted rclone configuration for stronger
protection on disk.

Cloud Drive token failures trigger one synchronized renewal and one replay of
the rejected request. Web access-token rotations are saved when returned by
IDrive. This is a proprietary web session, not OAuth refresh-token authorization.
When IDrive expires or revokes the web session, run `rclone config reconnect remote:`
and authorize again. Long-term session lifetime and concurrent token rotation
across separate rclone processes are not yet validated.

Manual endpoint/Cloud Drive token configuration remains available for existing
remotes. Manual mode does not renew expired tokens.

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
round-trips pass. Browser-assisted authorization and Cloud Drive token renewal
are implemented; HTTP transport integration remains unfinished. The private API may change.
