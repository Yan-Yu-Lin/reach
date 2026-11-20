# Design: handle-relative authorized_keys writes on Windows (daybreak P2-9)

Status: partial fix shipped 2026-11-21; full design not implemented. Not
tested on a Windows host.

## Shipped

`writeAuthorizedKeysFile` (Windows) refuses the write when any
user-controlled component (`<home>`, `.ssh`, `authorized_keys`) is a symlink
or reparse point (junction, mount point), checked with `Lstat` before
writing and again just before the rename. The DACL is set on the temp file
before content is written (astra P1-02).

## Remaining gap

These are pathname checks. A target user who can write `<home>` can swap
`.ssh` for a junction between the last check and `MoveFileEx`. The window is
small and the user needs a reliable race, but it is not closed.

## Full design

1. Open each user-controlled directory with
   `CreateFile(FILE_FLAG_BACKUP_SEMANTICS | FILE_FLAG_OPEN_REPARSE_POINT)`,
   check `FILE_ATTRIBUTE_REPARSE_POINT` from
   `GetFileInformationByHandle`, and keep the handle open (deny delete/
   rename sharing) for the duration of the write.
2. Create the temp file relative to the verified `.ssh` handle with
   `NtCreateFile` (`ObjectAttributes.RootDirectory = handle`).
3. Replace with `SetFileInformationByHandle(FileRenameInfoEx)` using
   `RootDirectory = .ssh handle` and `FILE_RENAME_FLAG_REPLACE_IF_EXISTS`,
   so the rename cannot be redirected.
4. Verify the final path with `GetFinalPathNameByHandle` equals the
   expected path.

Needs a Windows CI runner to test; ~200 lines in `golang.org/x/sys/windows`.

## Round 3 status (daybreak P2-G, 2026-11-21)

Unchanged: the pathname checks narrow but do not close the race; nothing
here has run on Windows. Closing it needs a Windows test machine, which
Reach does not have yet. Test plan once one exists:

1. Unit: the handle-relative writer refuses when `.ssh` is a junction at
   open time.
2. Race PoC: a loop that swaps `<home>\.ssh` between a directory and a
   junction to `C:\ProgramData\ssh` while `sync_keys` runs 10 000 times;
   `administrators_authorized_keys` must never change.
3. ACL: `icacls` on the result equals the expected DACL (SYSTEM,
   Administrators, the user), inherited entries removed.

Until then: Windows agents are released only in Stage 5, after this and
the sshd policy (`setup.ps1` without `-InstallOpenSSH`) are checked on a
recoverable Windows machine.
