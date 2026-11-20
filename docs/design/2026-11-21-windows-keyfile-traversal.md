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
